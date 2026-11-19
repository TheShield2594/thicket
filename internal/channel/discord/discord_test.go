package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/theshield2594/thicket/internal/channel"
)

// fakeDiscord is a tiny Discord REST API: it records every request and
// answers the few the adapter makes.
type fakeDiscord struct {
	mu   sync.Mutex
	reqs []request
	// hookApp is the application_id of webhooks it creates ("" means a
	// webhook no application owns).
	hookApp string
}

type request struct {
	method, path, query string
	body                map[string]any
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	path := strings.TrimPrefix(r.URL.Path, "/api/v"+discordgo.APIVersion)
	f.mu.Lock()
	f.reqs = append(f.reqs, request{r.Method, path, r.URL.RawQuery, body})
	app := f.hookApp
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && strings.HasSuffix(path, "/callback"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "GET" && strings.HasSuffix(path, "/webhooks"):
		w.Write([]byte(`[]`))
	case r.Method == "POST" && strings.HasSuffix(path, "/webhooks"):
		json.NewEncoder(w).Encode(map[string]any{"id": "h1", "token": "htok", "name": webhookName, "application_id": app, "user": map[string]any{"id": "bot"}})
	case r.Method == "POST" && strings.HasSuffix(path, "/messages"):
		w.Write([]byte(`{"id": "m1"}`))
	case r.Method == "POST" && strings.HasPrefix(path, "/webhooks/"):
		w.Write([]byte(`{"id": "w1"}`))
	case r.Method == "PATCH":
		w.Write([]byte(`{"id": "m1"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message": "not found", "code": 0}`))
	}
}

func (f *fakeDiscord) take() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

// redirect sends every request to the fake instead of discord.com.
type redirect struct{ to *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host, req.Host = r.to.Scheme, r.to.Host, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newTest(t *testing.T) (*Adapter, *fakeDiscord) {
	f := &fakeDiscord{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: redirect{u}}
	s.State.User = &discordgo.User{ID: "bot"}
	a := New("test-token", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.s = s
	a.chanInfo["guild"] = &discordgo.Channel{ID: "guild", Type: discordgo.ChannelTypeGuildText}
	a.chanInfo["dm"] = &discordgo.Channel{ID: "dm", Type: discordgo.ChannelTypeDM}
	return a, f
}

var approveDeny = []channel.Action{
	{ID: "approval:7:approve", Label: "Approve", Style: channel.ActionPrimary},
	{ID: "approval:7:deny", Label: "Deny", Style: channel.ActionDanger},
}

const wantButtons = `[{"components":[{"custom_id":"approval:7:approve","disabled":false,"label":"Approve","style":1,"type":2},{"custom_id":"approval:7:deny","disabled":false,"label":"Deny","style":4,"type":2}],"type":1}]`

func buttons(t *testing.T, r request) string {
	t.Helper()
	b, _ := json.Marshal(r.body["components"])
	return string(b)
}

func TestSendActionsInDM(t *testing.T) {
	a, f := newTest(t)
	ids, err := a.SendActions(context.Background(), "dm", channel.Persona{Name: "Pip"}, "🔒 I need your OK", approveDeny)
	if err != nil || len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("send: %v %v", ids, err)
	}
	reqs := f.take()
	if len(reqs) != 1 || reqs[0].path != "/channels/dm/messages" {
		t.Fatalf("requests: %+v", reqs)
	}
	if got := buttons(t, reqs[0]); got != wantButtons {
		t.Fatalf("buttons:\n got %s\nwant %s", got, wantButtons)
	}
	emb, _ := json.Marshal(reqs[0].body["embeds"])
	if !strings.Contains(string(emb), `"name":"Pip"`) || !strings.Contains(string(emb), "I need your OK") {
		t.Fatalf("persona embed: %s", emb)
	}
}

// In a server channel, buttons go through the persona webhook only when
// an application owns it; otherwise through the bot.
func TestSendActionsInServerChannel(t *testing.T) {
	a, f := newTest(t)
	f.hookApp = "app"
	if _, err := a.SendActions(context.Background(), "guild", channel.Persona{Name: "Pip"}, "🔒 I need your OK", approveDeny); err != nil {
		t.Fatal(err)
	}
	reqs := f.take()
	last := reqs[len(reqs)-1]
	if last.path != "/webhooks/h1/htok" || last.body["username"] != "Pip" || buttons(t, last) != wantButtons {
		t.Fatalf("app-owned webhook not used: %+v", reqs)
	}

	b, f2 := newTest(t)
	if _, err := b.SendActions(context.Background(), "guild", channel.Persona{Name: "Pip"}, "🔒 I need your OK", approveDeny); err != nil {
		t.Fatal(err)
	}
	reqs = f2.take()
	last = reqs[len(reqs)-1]
	if last.path != "/channels/guild/messages" || buttons(t, last) != wantButtons {
		t.Fatalf("buttons should go through the bot: %+v", reqs)
	}
	for _, r := range reqs {
		if strings.HasPrefix(r.path, "/webhooks/") {
			t.Fatalf("posted through a webhook no application owns: %+v", r)
		}
	}
}

func interaction(userID string, bot bool) *discordgo.Interaction {
	return &discordgo.Interaction{
		ID: "i1", AppID: "app", Token: "itok", Type: discordgo.InteractionMessageComponent,
		GuildID: "g", ChannelID: "guild",
		Member:  &discordgo.Member{User: &discordgo.User{ID: userID, Username: "sam", GlobalName: "Sam", Bot: bot}},
		Data:    discordgo.MessageComponentInteractionData{CustomID: "approval:7:approve", ComponentType: discordgo.ButtonComponent},
		Message: &discordgo.Message{ID: "m1", Embeds: []*discordgo.MessageEmbed{{Description: "🔒 I need your OK"}}},
	}
}

func TestButtonPress(t *testing.T) {
	a, f := newTest(t)
	var got []channel.Press
	a.OnPress(func(_ context.Context, p channel.Press) channel.PressReply {
		got = append(got, p)
		if p.UserID != "42" {
			return channel.PressReply{Notice: "This isn't yours to approve."}
		}
		return channel.PressReply{Outcome: "✅ Approved"}
	})
	ctx := context.Background()

	// Someone else: acknowledged, told privately, message untouched.
	a.press(ctx, interaction("7", false))
	reqs := f.take()
	if len(got) != 1 || got[0] != (channel.Press{Channel: "discord", ChatID: "guild", MessageID: "m1", UserID: "7", UserName: "Sam", ActionID: "approval:7:approve"}) {
		t.Fatalf("press: %+v", got)
	}
	if len(reqs) != 2 || reqs[0].path != "/interactions/i1/itok/callback" || reqs[0].body["type"] != float64(discordgo.InteractionResponseDeferredMessageUpdate) {
		t.Fatalf("not acknowledged first: %+v", reqs)
	}
	if reqs[1].method != "POST" || reqs[1].path != "/webhooks/app/itok" || reqs[1].body["content"] != "This isn't yours to approve." ||
		reqs[1].body["flags"] != float64(discordgo.MessageFlagsEphemeral) {
		t.Fatalf("notice should be an ephemeral follow-up: %+v", reqs[1])
	}

	// The owner: the message is edited to show the outcome, buttons gone.
	a.press(ctx, interaction("42", false))
	reqs = f.take()
	if len(reqs) != 2 || reqs[1].method != "PATCH" || reqs[1].path != "/webhooks/app/itok/messages/@original" {
		t.Fatalf("message not settled: %+v", reqs)
	}
	edit, _ := json.Marshal(reqs[1].body)
	if !strings.Contains(string(edit), `"components":[]`) || !strings.Contains(string(edit), `"footer":{"text":"✅ Approved"}`) ||
		!strings.Contains(string(edit), "I need your OK") {
		t.Fatalf("edit: %s", edit)
	}

	// Bots never press.
	a.press(ctx, interaction("8", true))
	if len(got) != 2 {
		t.Fatalf("a bot's press reached the handler: %+v", got)
	}
	if reqs := f.take(); len(reqs) != 1 {
		t.Fatalf("a bot's press should only be acknowledged: %+v", reqs)
	}
}

func TestSettledWithoutEmbeds(t *testing.T) {
	e := settled(&discordgo.Message{Content: "**Pip:** 🔒 I need your OK"}, "✖️ Denied")
	if len(*e.Components) != 0 || len(*e.Embeds) != 1 || (*e.Embeds)[0].Description != "✖️ Denied" {
		t.Fatalf("settled: %+v", e)
	}
}
