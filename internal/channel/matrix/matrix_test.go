package matrix

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
)

const (
	token = "syt_Ym90_SECRETsecretSECRETsecret_0abc12"
	botID = "@thicket:example.org"
)

// fakeHS is a tiny homeserver: /sync answers from a queue of responses
// (then waits, as a long poll does), and every other call is recorded.
type fakeHS struct {
	t     *testing.T
	mu    sync.Mutex
	syncs []map[string]any
	// syncQs are the query strings of the /sync calls.
	syncQs []string
	calls  []call
	media  map[string]string // media id -> content
	// v1Media is whether the authenticated media API exists.
	v1Media bool
	n       int
	// whoamiDown answers that many whoami calls with a 503 first.
	whoamiDown int
}

type call struct {
	Method, Path string
	Body         map[string]any
}

func newFake(t *testing.T) (*fakeHS, *httptest.Server) {
	f := &fakeHS{t: t, media: map[string]string{}, v1Media: true}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeHS) queue(r map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, r)
}

func (f *fakeHS) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(401)
		w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token"}`))
		return
	}
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/_matrix/client/v1/media/download/"), strings.HasPrefix(path, "/_matrix/media/v3/download/"):
		if strings.HasPrefix(path, "/_matrix/client/v1/") && !f.v1Media {
			w.WriteHeader(404)
			w.Write([]byte(`{"errcode":"M_UNRECOGNIZED"}`))
			return
		}
		parts := strings.Split(path, "/")
		f.mu.Lock()
		body, ok := f.media[parts[len(parts)-1]]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte(body))
		return
	case path == "/_matrix/client/v3/account/whoami":
		f.mu.Lock()
		down := f.whoamiDown > 0
		if down {
			f.whoamiDown--
		}
		f.mu.Unlock()
		if down {
			w.WriteHeader(503)
			w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"starting up"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user_id": botID})
		return
	case strings.HasPrefix(path, "/_matrix/client/v3/profile/"):
		json.NewEncoder(w).Encode(map[string]any{"displayname": "Thicket"})
		return
	case path == "/_matrix/client/v3/sync":
		f.mu.Lock()
		f.syncQs = append(f.syncQs, r.URL.RawQuery)
		var next map[string]any
		if len(f.syncs) > 0 {
			next, f.syncs = f.syncs[0], f.syncs[1:]
		}
		f.n++
		n := f.n
		f.mu.Unlock()
		if next == nil {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			next = map[string]any{}
		}
		next["next_batch"] = "batch" + string(rune('0'+n%10))
		json.NewEncoder(w).Encode(next)
		return
	}
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, path, body})
	f.n++
	n := f.n
	f.mu.Unlock()
	switch {
	case strings.Contains(path, "/send/"), strings.Contains(path, "/redact/"):
		json.NewEncoder(w).Encode(map[string]any{"event_id": "$sent" + string(rune('a'+n%26))})
	case strings.HasSuffix(path, "/createRoom"):
		json.NewEncoder(w).Encode(map[string]any{"room_id": "!dm:example.org"})
	case strings.Contains(path, "/join/"):
		json.NewEncoder(w).Encode(map[string]any{"room_id": "joined"})
	default:
		w.Write([]byte(`{}`))
	}
}

func (f *fakeHS) callsTo(sub string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if strings.Contains(c.Path, sub) {
			out = append(out, c)
		}
	}
	return out
}

func msg(id, sender string, content map[string]any) map[string]any {
	return map[string]any{"type": "m.room.message", "event_id": id, "sender": sender, "content": content}
}

func joined(members int, events ...map[string]any) map[string]any {
	return map[string]any{"summary": map[string]any{"m.joined_member_count": members}, "timeline": map[string]any{"events": events}}
}

func rooms(join map[string]any) map[string]any {
	return map[string]any{"rooms": map[string]any{"join": join}}
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// run starts the adapter and collects what it delivers.
func run(t *testing.T, a *Adapter) (chan channel.Incoming, context.CancelFunc) {
	t.Helper()
	got := make(chan channel.Incoming, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx, func(_ context.Context, in channel.Incoming) { got <- in }); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for !a.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got, cancel
}

func next(t *testing.T, got chan channel.Incoming) channel.Incoming {
	t.Helper()
	select {
	case in := <-got:
		return in
	case <-time.After(3 * time.Second):
		t.Fatal("no message delivered")
	}
	return channel.Incoming{}
}

func none(t *testing.T, got chan channel.Incoming) {
	t.Helper()
	select {
	case in := <-got:
		t.Fatalf("unexpected delivery: %+v", in)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDMsAndMentions(t *testing.T) {
	f, srv := newFake(t)
	// The first sync's history is not answered.
	f.queue(rooms(map[string]any{"!dm:x": joined(2, msg("$old", "@alice:x", map[string]any{"msgtype": "m.text", "body": "from before"}))}))
	f.queue(rooms(map[string]any{
		"!dm:x": joined(2,
			msg("$own", botID, map[string]any{"msgtype": "m.text", "body": "my own"}),
			msg("$n", "@otherbot:x", map[string]any{"msgtype": "m.notice", "body": "a bot's notice"}),
			msg("$1", "@alice:x", map[string]any{"msgtype": "m.text", "body": "hello there"})),
	}))
	a := New(srv.URL, token, nil, logger())
	got, _ := run(t, a)

	in := next(t, got)
	if in.Channel != "matrix" || in.ChatID != "!dm:x" || in.MessageID != "$1" || in.UserID != "@alice:x" || in.Text != "hello there" || !in.IsDM {
		t.Fatalf("DM: %+v", in)
	}
	if in.ChatRef() != "matrix:!dm:x" {
		t.Errorf("chat ref %q", in.ChatRef())
	}
	none(t, got)

	// In a room: a mention (by m.mentions with the display name in the
	// body), a message to nobody, a reply to the bot.
	f.queue(rooms(map[string]any{
		"!room:x": joined(5,
			map[string]any{"type": "m.room.member", "state_key": "@bob:x", "sender": "@bob:x", "event_id": "$m", "content": map[string]any{"membership": "join", "displayname": "Bob"}},
			msg("$2", "@bob:x", map[string]any{"msgtype": "m.text", "body": "Thicket: what's up?", "m.mentions": map[string]any{"user_ids": []string{botID}}}),
			msg("$3", "@bob:x", map[string]any{"msgtype": "m.text", "body": "just chatting"})),
	}))
	// Messages are handled concurrently, so they may arrive in any order.
	byID := map[string]channel.Incoming{}
	for range 2 {
		in := next(t, got)
		byID[in.MessageID] = in
	}
	if in := byID["$2"]; in.IsDM || !in.Mentioned || in.Text != "what's up?" || in.UserName != "Bob" {
		t.Fatalf("mention: %+v", in)
	}
	if in := byID["$3"]; in.Mentioned || in.Text != "just chatting" {
		t.Fatalf("plain room message: %+v", in)
	}

	ids, err := a.SendIDs(context.Background(), "!room:x", channel.Persona{Name: "Pip"}, "**hi** <there>")
	if err != nil || len(ids) != 1 {
		t.Fatalf("send: %v %v", ids, err)
	}
	f.queue(rooms(map[string]any{
		"!room:x": joined(5, msg("$4", "@bob:x", map[string]any{"msgtype": "m.text", "body": "> <@thicket:example.org> Pip: hi\n\nthanks!",
			"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": ids[0]}}})),
	}))
	in = next(t, got)
	if !in.Mentioned || in.Text != "thanks!" || in.ReplyTo != ids[0] {
		t.Fatalf("reply to the bot: %+v", in)
	}

	// The first sync asked for little history; later ones long-poll.
	f.mu.Lock()
	qs := f.syncQs
	f.mu.Unlock()
	if !strings.Contains(qs[0], "timeout=0") || strings.Contains(qs[0], "since=") || !strings.Contains(qs[1], "since=") || !strings.Contains(qs[1], "timeout=30000") {
		t.Errorf("sync queries: %q", qs[:2])
	}
}

func TestSendLabelsWithSprigName(t *testing.T) {
	f, srv := newFake(t)
	a := New(srv.URL, token, nil, logger())
	if err := a.Send(context.Background(), "!r:x", channel.Persona{Name: "Pip"}, "**bold** & `code`\nnext"); err != nil {
		t.Fatal(err)
	}
	c := f.callsTo("/send/m.room.message/")
	if len(c) != 1 || c[0].Method != http.MethodPut || !strings.HasPrefix(c[0].Path, "/_matrix/client/v3/rooms/%21r:x/send/") {
		t.Fatalf("calls: %+v", c)
	}
	b := c[0].Body
	if b["body"] != "Pip:\n**bold** & `code`\nnext" || b["msgtype"] != "m.text" ||
		b["formatted_body"] != "<strong>Pip</strong><br><strong>bold</strong> &amp; <code>code</code><br>next" {
		t.Errorf("content: %+v", b)
	}
	if m, ok := b["m.mentions"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("sprig replies should ping nobody: %+v", b["m.mentions"])
	}

	// A long reply is split, and only the first part is labelled.
	if _, err := a.SendIDs(context.Background(), "!r:x", channel.Persona{Name: "Pip"}, strings.Repeat("word ", 5000)); err != nil {
		t.Fatal(err)
	}
	c = f.callsTo("/send/m.room.message/")
	if len(c) != 3 || !strings.HasPrefix(c[1].Body["body"].(string), "Pip:") || strings.HasPrefix(c[2].Body["body"].(string), "Pip:") {
		t.Errorf("split: %d calls", len(c))
	}

	if err := a.Delete(context.Background(), "!r:x", "$ev"); err != nil {
		t.Fatal(err)
	}
	if c := f.callsTo("/redact/"); len(c) != 1 || !strings.Contains(c[0].Path, "/rooms/%21r:x/redact/$ev/") {
		t.Errorf("redact: %+v", c)
	}
}

func TestEncryptedRoomsRefused(t *testing.T) {
	f, srv := newFake(t)
	enc := map[string]any{"type": "m.room.encryption", "state_key": "", "sender": "@alice:x", "content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}}
	inviteMe := func(direct bool) map[string]any {
		return map[string]any{"type": "m.room.member", "state_key": botID, "sender": "@alice:x", "content": map[string]any{"membership": "invite", "is_direct": direct}}
	}
	f.queue(map[string]any{"rooms": map[string]any{"invite": map[string]any{
		"!secret:x": map[string]any{"invite_state": map[string]any{"events": []any{enc, inviteMe(true)}}},
		"!plain:x":  map[string]any{"invite_state": map[string]any{"events": []any{inviteMe(true)}}},
	}}})
	a := New(srv.URL, token, nil, logger())
	got, _ := run(t, a)

	leaves := f.callsTo("/leave")
	if len(leaves) != 1 || !strings.Contains(leaves[0].Path, "%21secret:x") || leaves[0].Body["reason"] != EncryptedNotice {
		t.Fatalf("encrypted invite should be declined with the reason: %+v", leaves)
	}
	if joins := f.callsTo("/join/"); len(joins) != 1 || !strings.Contains(joins[0].Path, "%21plain:x") {
		t.Fatalf("plain invite should be joined: %+v", joins)
	}

	// A joined room that turns encryption on: the bot says why and leaves,
	// and what's said there is not delivered.
	f.queue(rooms(map[string]any{"!plain:x": joined(2, enc,
		map[string]any{"type": "m.room.encrypted", "sender": "@alice:x", "event_id": "$e", "content": map[string]any{}})}))
	none(t, got)
	deadline := time.Now().Add(3 * time.Second)
	for len(f.callsTo("/leave")) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if l := f.callsTo("/leave"); len(l) != 2 || !strings.Contains(l[1].Path, "%21plain:x") {
		t.Fatalf("should leave the room that turned encryption on: %+v", l)
	}
	sends := f.callsTo("/send/")
	if len(sends) != 1 || sends[0].Body["body"] != EncryptedNotice {
		t.Errorf("should say why it leaves: %+v", sends)
	}

	// The DM from the invite is reused for SendDirect... but it was left,
	// so a new one is made.
	room, err := a.SendDirect(context.Background(), "@alice:x", channel.Persona{Name: "Juni"}, "hi")
	if err != nil || room != "!dm:example.org" {
		t.Fatalf("SendDirect: %q %v", room, err)
	}
}

func TestSendDirectReusesOrCreatesRoom(t *testing.T) {
	f, srv := newFake(t)
	f.queue(map[string]any{"account_data": map[string]any{"events": []any{
		map[string]any{"type": "m.direct", "content": map[string]any{"@alice:x": []string{"!known:x"}}},
	}}})
	a := New(srv.URL, token, nil, logger())
	run(t, a)

	room, err := a.SendDirect(context.Background(), "@alice:x", channel.Persona{Name: "Juni"}, "hi")
	if err != nil || room != "!known:x" || len(f.callsTo("/createRoom")) != 0 {
		t.Fatalf("known DM: %q %v", room, err)
	}
	room, err = a.SendDirect(context.Background(), "@carol:x", channel.Persona{Name: "Juni"}, "hi")
	if err != nil || room != "!dm:example.org" {
		t.Fatalf("new DM: %q %v", room, err)
	}
	cr := f.callsTo("/createRoom")
	if len(cr) != 1 || cr[0].Body["is_direct"] != true || cr[0].Body["preset"] != "trusted_private_chat" {
		t.Fatalf("createRoom: %+v", cr)
	}
	ad := f.callsTo("/account_data/m.direct")
	if len(ad) != 1 || ad[0].Body["@carol:x"] == nil || ad[0].Body["@alice:x"] == nil {
		t.Errorf("m.direct should keep both DMs: %+v", ad)
	}
	// The next one reuses it.
	if room, _ = a.SendDirect(context.Background(), "@carol:x", channel.Persona{}, "again"); room != "!dm:example.org" || len(f.callsTo("/createRoom")) != 1 {
		t.Errorf("should reuse the DM: %q", room)
	}
}

func TestAttachments(t *testing.T) {
	for _, v1 := range []bool{true, false} {
		f, srv := newFake(t)
		f.v1Media = v1
		f.media["abc"] = "pngbytes"
		f.queue(map[string]any{})
		f.queue(rooms(map[string]any{"!dm:x": joined(2,
			msg("$img", "@alice:x", map[string]any{"msgtype": "m.image", "body": "cat.png", "url": "mxc://example.org/abc",
				"info": map[string]any{"mimetype": "image/png", "size": 8}}),
			msg("$cap", "@alice:x", map[string]any{"msgtype": "m.file", "body": "what's this?", "filename": "notes.pdf", "url": "mxc://example.org/abc"}))}))
		a := New(srv.URL, token, nil, logger())
		got, _ := run(t, a)
		byID := map[string]channel.Incoming{}
		for range 2 {
			in := next(t, got)
			byID[in.MessageID] = in
		}
		in := byID["$img"]
		if in.Text != "" || len(in.Attachments) != 1 || in.Attachments[0].Name != "cat.png" || in.Attachments[0].ContentType != "image/png" {
			t.Fatalf("image: %+v", in)
		}
		b, ct, err := in.Attachments[0].Open(context.Background(), 100)
		if err != nil || string(b) != "pngbytes" || ct != "image/png" {
			t.Fatalf("download (v1 %v): %q %q %v", v1, b, ct, err)
		}
		if _, _, err := in.Attachments[0].Open(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("size limit: %v", err)
		}
		in = byID["$cap"]
		if in.Text != "what's this?" || len(in.Attachments) != 1 || in.Attachments[0].Name != "notes.pdf" {
			t.Fatalf("file with caption: %+v", in)
		}
	}
}

func TestReactions(t *testing.T) {
	f, srv := newFake(t)
	f.queue(map[string]any{})
	f.queue(rooms(map[string]any{"!dm:x": joined(2, map[string]any{"type": "m.reaction", "sender": "@alice:x", "event_id": "$r",
		"content": map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$target", "key": "🧠"}}})}))
	a := New(srv.URL, token, nil, logger())
	reacts := make(chan channel.Reaction, 1)
	a.OnReaction(func(_ context.Context, r channel.Reaction) { reacts <- r })
	run(t, a)
	select {
	case r := <-reacts:
		if r.Channel != "matrix" || r.ChatID != "!dm:x" || r.MessageID != "$target" || r.Emoji != "🧠" || r.UserID != "@alice:x" || !r.IsDM {
			t.Errorf("reaction: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reaction")
	}
}

func TestBadToken(t *testing.T) {
	_, srv := newFake(t)
	a := New(srv.URL, "wrong", nil, logger())
	err := a.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "didn't accept that access token") {
		t.Fatalf("Check: %v", err)
	}
	if err := a.Run(context.Background(), func(context.Context, channel.Incoming) {}); err == nil {
		t.Fatal("Run should fail with a bad token")
	}
	if a.Connected() {
		t.Error("connected with a bad token")
	}
}

func TestListeningRooms(t *testing.T) {
	f, srv := newFake(t)
	f.queue(map[string]any{})
	f.queue(rooms(map[string]any{"!all:x": joined(9, msg("$1", "@bob:x", map[string]any{"msgtype": "m.text", "body": "anyone?"}))}))
	a := New(srv.URL, token, []string{"!all:x"}, logger())
	got, _ := run(t, a)
	if in := next(t, got); !in.Listening || in.Mentioned || in.IsDM {
		t.Fatalf("listening room: %+v", in)
	}
}

// A homeserver that's still starting is waited for, not given up on.
func TestRunWaitsForHomeserver(t *testing.T) {
	f, srv := newFake(t)
	f.whoamiDown = 1
	f.queue(map[string]any{})
	a := New(srv.URL, token, nil, logger())
	run(t, a)
	deadline := time.Now().Add(10 * time.Second)
	for !a.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("never connected after the homeserver came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Two sends to someone new at once open one DM room.
func TestSendDirectConcurrentOpensOneRoom(t *testing.T) {
	f, srv := newFake(t)
	f.queue(map[string]any{})
	a := New(srv.URL, token, nil, logger())
	run(t, a)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.SendDirect(context.Background(), "@dana:x", channel.Persona{}, "hi"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(f.callsTo("/createRoom")); n != 1 {
		t.Fatalf("%d rooms created", n)
	}
}

func TestSplitLongBacktickRun(t *testing.T) {
	const n = 100
	for _, text := range []string{
		strings.Repeat("`", 3*n),
		"hello " + strings.Repeat("`", 3*n),
		strings.Repeat("word ", 60),
	} {
		done := make(chan []string, 1)
		go func() { done <- split(text, n) }()
		select {
		case parts := <-done:
			for _, p := range parts {
				if p == "" {
					t.Fatalf("empty chunk splitting %q...", text[:10])
				}
			}
			if len(parts) < 2 {
				t.Fatalf("not split: %d parts", len(parts))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("split never finished on %q...", text[:10])
		}
	}
}
