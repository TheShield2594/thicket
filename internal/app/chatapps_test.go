package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
)

// botChannel is a chat app built from a token: it comes online when Run
// starts, unless the token is in bad.
type botChannel struct {
	*fakeChannel
	token  string
	fail   error
	online atomic.Bool
}

func (b *botChannel) Run(ctx context.Context, h channel.Handler) error {
	if b.fail != nil {
		return b.fail
	}
	b.online.Store(true)
	defer b.online.Store(false)
	<-ctx.Done()
	return nil
}
func (b *botChannel) Connected() bool { return b.online.Load() }
func (b *botChannel) Bot() (channel.Bot, bool) {
	return channel.Bot{Name: "thicketbot", URL: "https://discord.example/add?bot=" + b.token[:4]}, b.online.Load()
}

// chatRig is a running Thicket whose owner talks on Telegram, and which
// can connect other chat apps from a token.
type chatRig struct {
	a   *App
	tg  *fakeChannel
	fm  *fakeModel
	ctx context.Context

	mu    sync.Mutex
	built []*botChannel
	bad   map[string]error // token -> why it fails
}

func (r *chatRig) last() *botChannel {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.built) == 0 {
		return nil
	}
	return r.built[len(r.built)-1]
}

func (r *chatRig) say(text string) []sent {
	r.a.Engine.HandleIncoming(r.ctx, channel.Incoming{Channel: "telegram", ChatID: "777", MessageID: fmt.Sprint(time.Now().UnixNano()),
		UserID: "777", UserName: "Sam", Text: text, IsDM: true})
	return r.tg.take()
}

func newChatRig(t *testing.T) *chatRig {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("THICKET_DATA_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("THICKET_DATA_DIR") })
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CharactersDir = filepath.Join(dir, "characters")
	r := &chatRig{tg: &fakeChannel{name: "telegram"}, fm: &fakeModel{}, bad: map[string]error{}}
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.a, err = New(ctx, cfg, "test", Options{
		Model: r.fm, KeyCheck: func(context.Context, string) error { return nil }, LogOut: io.Discard, NoHTTP: true,
		Channels: func(*slog.Logger) []channel.Channel { return []channel.Channel{r.tg} },
		NewChannel: func(name, token string, _ *slog.Logger) channel.Channel {
			if name != "discord" {
				return nil
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			b := &botChannel{fakeChannel: &fakeChannel{}, token: token, fail: r.bad[token]}
			r.built = append(r.built, b)
			return b
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = r.a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; r.a.Close() })
	for i := 0; ; i++ {
		r.a.mu.Lock()
		up := r.a.runCtx != nil
		r.a.mu.Unlock()
		if up {
			break
		}
		if i > 500 {
			t.Fatal("Thicket didn't start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.say("hello?")
	r.say("sk-or-v1-" + strings.Repeat("ab12", 16))
	return r
}

// discordToken is shaped like a real bot token (the id in base64 first).
func discordToken(id, sig string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(id)) + ".Gh8aZ1." + strings.Repeat(sig, 10)
}

func modelSaw(fm *fakeModel, s string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, s) {
				return true
			}
		}
	}
	return false
}

// An admin pastes a Discord bot token in a DM: Discord connects while
// Thicket runs, the token is stored, and nobody is sent to an env file.
func TestPastedDiscordTokenConnectsDiscord(t *testing.T) {
	r := newChatRig(t)
	tok := discordToken("123456789012345678", "aB3")
	calls := r.fm.calls

	out := r.say("here's the bot token " + tok)
	if len(out) != 1 || !strings.Contains(out[0].text, "Discord is connected as **thicketbot**") ||
		!strings.Contains(out[0].text, "https://discord.example/add?bot=") || !strings.Contains(out[0].text, "link my Discord") {
		t.Fatalf("reply: %+v", out)
	}
	if strings.Contains(out[0].text, tok) || modelSaw(r.fm, tok) || r.fm.calls != calls {
		t.Fatal("the token reached the chat or the model")
	}
	assertNoLeak(t, r.a, tok)
	if got, err := r.a.Secrets.Get(r.ctx, secrets.DiscordToken); err != nil || got != tok {
		t.Fatalf("stored token: %q %v", got, err)
	}
	dc := r.a.Engine.Channels()["discord"]
	if dc == nil || !dc.Connected() || dc != channel.Channel(r.last()) {
		t.Fatalf("discord not registered: %v", dc)
	}
	if c, ok := r.a.health(r.ctx)["channel.discord"]; !ok || !c.OK {
		t.Fatalf("health: %+v", r.a.health(r.ctx))
	}
	if !r.a.webStatus(r.ctx, false).Channels["discord"] {
		t.Fatal("the web app doesn't show Discord as connected")
	}

	// The sprig knows what happened on its next reply.
	r.say("thanks!")
	if !modelSaw(r.fm, "Thicket connected Discord as **thicketbot**") {
		t.Fatal("the model wasn't told Discord is connected")
	}

	// The same token again changes nothing; a new one replaces the bot.
	if out := r.say(tok); len(out) != 1 || !strings.Contains(out[0].text, "Discord is connected") || len(r.built) != 1 {
		t.Fatalf("same token again: %+v, built %d", out, len(r.built))
	}
	first, tok2 := r.last(), discordToken("987654321098765432", "Zx9")
	if out := r.say(tok2); len(out) != 1 || !strings.Contains(out[0].text, "Discord is connected") {
		t.Fatalf("new token: %+v", out)
	}
	if first.Connected() || !r.last().Connected() || r.a.Engine.Channels()["discord"] != channel.Channel(r.last()) {
		t.Fatal("the old bot wasn't replaced")
	}
	if got, _ := r.a.Secrets.Get(r.ctx, secrets.DiscordToken); got != tok2 {
		t.Fatal("the new token wasn't stored")
	}
}

// A token Discord refuses is not stored, the reason is passed on, and a
// working connection is put back.
func TestBadDiscordTokenIsNotKept(t *testing.T) {
	r := newChatRig(t)
	bad := discordToken("111111111111111111", "bad")
	r.bad[bad] = errors.New("Discord refused the connection because the Message Content intent is off")

	out := r.say(bad)
	if len(out) != 1 || !strings.Contains(out[0].text, "Message Content intent is off") || !strings.Contains(out[0].text, "haven't kept") {
		t.Fatalf("reply: %+v", out)
	}
	if r.a.Secrets.Has(r.ctx, secrets.DiscordToken) || r.a.Engine.Channels()["discord"] != nil {
		t.Fatal("a failed token left something behind")
	}
	if _, ok := r.a.health(r.ctx)["channel.discord"]; ok {
		t.Fatal("a failed connection is in /health")
	}
	assertNoLeak(t, r.a, bad)

	good := discordToken("123456789012345678", "aB3")
	r.say(good)
	if out := r.say(bad); len(out) != 1 || !strings.Contains(out[0].text, "didn't work") {
		t.Fatalf("bad token over a good one: %+v", out)
	}
	dc := r.a.Engine.Channels()["discord"]
	if dc == nil || !dc.Connected() || dc.(*botChannel).token != good {
		t.Fatalf("the working connection wasn't restored: %+v", dc)
	}
	if got, _ := r.a.Secrets.Get(r.ctx, secrets.DiscordToken); got != good {
		t.Fatal("the stored token changed")
	}
}

// Juni's tool: it asks for the token (which then connects), and connects
// from a token that was stored under another name earlier.
func TestConnectChatAppTool(t *testing.T) {
	r := newChatRig(t)

	// request_secret sends chat tokens to connect_chat_app.
	r.say("ask for the secret discord_bot_token")
	if !modelSaw(r.fm, "goes through connect_chat_app") {
		t.Fatal("request_secret accepted a chat bot token")
	}

	r.say("connect discord")
	if !modelSaw(r.fm, "Message Content Intent") || !modelSaw(r.fm, "no restart") {
		t.Fatal("the tool didn't explain how to get a token")
	}
	// The awaited token has an unusual shape: it is still used to connect.
	odd := "odd-shaped-bot-token-0123456789"
	if out := r.say(odd); len(out) != 1 || !strings.Contains(out[0].text, "Discord is connected") {
		t.Fatalf("awaited token: %+v", out)
	}
	if got, _ := r.a.Secrets.Get(r.ctx, secrets.DiscordToken); got != odd {
		t.Fatalf("stored %q", got)
	}
	if modelSaw(r.fm, odd) {
		t.Fatal("the token reached the model")
	}
	r.say("connect discord")
	if !modelSaw(r.fm, "Discord is already connected as **thicketbot**") {
		t.Fatal("the tool didn't report the connection")
	}
}

func TestConnectChatAppFromStoredSecret(t *testing.T) {
	r := newChatRig(t)
	tok := discordToken("123456789012345678", "aB3")
	if err := r.a.Secrets.Put(r.ctx, "discord_bot_token", tok); err != nil {
		t.Fatal(err)
	}
	r.say("connect discord discord_bot_token")
	if !modelSaw(r.fm, "Discord is now connected as **thicketbot**") {
		t.Fatal("not connected from the stored secret")
	}
	if got, _ := r.a.Secrets.Get(r.ctx, secrets.DiscordToken); got != tok {
		t.Fatal("the token wasn't moved to discord_token")
	}
	if r.a.Secrets.Has(r.ctx, "discord_bot_token") {
		t.Fatal("the old name is still stored")
	}
	if dc := r.a.Engine.Channels()["discord"]; dc == nil || !dc.Connected() {
		t.Fatal("discord isn't up")
	}
}

// Members can't connect chat apps, and a token in a group chat is deleted.
func TestOnlyAdminsConnectChatApps(t *testing.T) {
	r := newChatRig(t)
	tok := discordToken("123456789012345678", "aB3")
	r.a.Engine.HandleIncoming(r.ctx, channel.Incoming{Channel: "telegram", ChatID: "group", MessageID: "g1", UserID: "777", UserName: "Sam", Text: tok, Mentioned: true})
	if out := r.tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "deleted that message") {
		t.Fatalf("group paste: %+v", out)
	}
	if r.a.Engine.Channels()["discord"] != nil || r.last() != nil {
		t.Fatal("a token in a group chat connected Discord")
	}
}

var _ llm.Client = (*fakeModel)(nil)
