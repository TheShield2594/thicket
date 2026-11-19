package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
)

const token = "123456:SECRET-token-value-abcdefghijklmnop"

// fakeAPI is a tiny Bot API: it serves queued updates once and records
// what was sent.
type fakeAPI struct {
	mu      sync.Mutex
	updates []map[string]any
	sent    []map[string]any
	// allowed is the allowed_updates of the last getUpdates.
	allowed []any
	// rejectHTML makes sendMessage refuse parse_mode HTML once.
	rejectHTML bool
	// noVoice refuses sendVoice, as for a user who blocks voice messages.
	noVoice bool
}

// multipartParams reads an upload's fields, and its file as "<field>"
// (the bytes), "<field>.name" and "<field>.type".
func multipartParams(t *testing.T, r *http.Request) map[string]any {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Errorf("bad multipart upload: %v", err)
		return map[string]any{}
	}
	p := map[string]any{}
	for k, v := range r.MultipartForm.Value {
		p[k] = v[0]
	}
	for k, fs := range r.MultipartForm.File {
		f, err := fs[0].Open()
		if err != nil {
			t.Error(err)
			continue
		}
		b, _ := io.ReadAll(f)
		f.Close()
		p[k], p[k+".name"], p[k+".type"] = string(b), fs[0].Filename, fs[0].Header.Get("Content-Type")
	}
	return p
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/bot"+token+"/") {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpegbytes"))
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		if method == r.URL.Path {
			w.WriteHeader(404)
			w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
			return
		}
		var p map[string]any
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			p = multipartParams(t, r)
		} else {
			json.NewDecoder(r.Body).Decode(&p)
		}
		ok := func(v any) { b, _ := json.Marshal(map[string]any{"ok": true, "result": v}); w.Write(b) }
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case "getMe":
			ok(map[string]any{"id": 999, "is_bot": true, "first_name": "Thicket", "username": "thicket_bot"})
		case "deleteWebhook", "sendChatAction", "deleteMessage":
			ok(true)
		case "getUpdates":
			f.allowed, _ = p["allowed_updates"].([]any)
			ups := f.updates
			f.updates = nil
			if ups == nil {
				ups = []map[string]any{}
			}
			ok(ups)
		case "sendMessage":
			if f.rejectHTML && p["parse_mode"] == "HTML" {
				f.rejectHTML = false
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
				return
			}
			f.sent = append(f.sent, p)
			ok(map[string]any{"message_id": 1})
		case "sendVoice", "sendAudio":
			if f.noVoice && method == "sendVoice" {
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: VOICE_MESSAGES_FORBIDDEN"}`))
				return
			}
			p["method"] = method
			f.sent = append(f.sent, p)
			ok(map[string]any{"message_id": 2})
		case "getFile":
			if p["file_id"] == "big" {
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`))
				return
			}
			ok(map[string]any{"file_path": "photos/p.jpg"})
		default:
			t.Errorf("unexpected method %s", method)
		}
	})
}

func newTest(t *testing.T, listen ...string) (*Adapter, *fakeAPI) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a := New(token, listen, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.BaseURL = srv.URL
	return a, f
}

func msg(id int, chat int64, chatType, text string, extra map[string]any) map[string]any {
	m := map[string]any{"message_id": id, "from": map[string]any{"id": 42, "first_name": "Sam"},
		"chat": map[string]any{"id": chat, "type": chatType}, "text": text}
	for k, v := range extra {
		m[k] = v
	}
	return map[string]any{"update_id": id, "message": m}
}

func TestReceive(t *testing.T) {
	a, f := newTest(t, "-100")
	f.updates = []map[string]any{
		msg(1, 42, "private", "/start", nil),
		msg(2, -5, "group", "@thicket_bot what's up?", map[string]any{"entities": []any{map[string]any{"type": "mention", "offset": 0, "length": 12}}}),
		msg(3, -5, "group", "just chatting", nil),
		msg(4, -100, "supergroup", "everyone hears this", nil),
		msg(5, 42, "private", "", map[string]any{"caption": "look 🌱", "photo": []any{
			map[string]any{"file_id": "small", "file_size": 10}, map[string]any{"file_id": "large", "file_size": 900}}}),
		{"update_id": 6, "message": map[string]any{"message_id": 6, "from": map[string]any{"id": 7, "is_bot": true}, "chat": map[string]any{"id": 42, "type": "private"}, "text": "bot"}},
		msg(7, -5, "group", "/help@thicket_bot", map[string]any{"entities": []any{map[string]any{"type": "bot_command", "offset": 0, "length": 17}}}),
		msg(8, 42, "private", "/start join-AbC_def-123", nil), // from an invite link
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan channel.Incoming, 10)
	go a.Run(ctx, func(_ context.Context, in channel.Incoming) { got <- in })

	want := map[string]func(channel.Incoming) bool{
		"1": func(in channel.Incoming) bool {
			return in.IsDM && in.Text == "Hello!" && in.UserID == "42" && in.UserName == "Sam"
		},
		"2": func(in channel.Incoming) bool { return !in.IsDM && in.Mentioned && in.Text == "what's up?" },
		"3": func(in channel.Incoming) bool { return !in.Mentioned && !in.Listening },
		"4": func(in channel.Incoming) bool { return in.Listening && in.ChatID == "-100" },
		"5": func(in channel.Incoming) bool {
			return in.Text == "look 🌱" && len(in.Attachments) == 1 && in.Attachments[0].Size == 900 && in.Attachments[0].ContentType == "image/jpeg"
		},
		"7": func(in channel.Incoming) bool { return in.Mentioned && in.Text == "/help" },
		"8": func(in channel.Incoming) bool { return in.IsDM && in.Text == "join-AbC_def-123" },
	}
	deadline := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case in := <-got:
			check, ok := want[in.MessageID]
			if !ok {
				t.Fatalf("unexpected message %s: %+v", in.MessageID, in)
			}
			if !check(in) {
				t.Errorf("message %s: %+v", in.MessageID, in)
			}
			if in.MessageID == "5" {
				body, ct, err := channel.Fetch(ctx, in.Attachments[0], 1000)
				if err != nil || string(body) != "jpegbytes" || ct != "image/jpeg" {
					t.Errorf("download: %q %q %v", body, ct, err)
				}
			}
			delete(want, in.MessageID)
		case <-deadline:
			t.Fatalf("messages never arrived: %v", want)
		}
	}
	if !a.Connected() {
		t.Error("not connected")
	}
}

func TestSendFormatsAndFallsBack(t *testing.T) {
	a, f := newTest(t)
	ctx := context.Background()
	if err := a.Send(ctx, "42", channel.Persona{Name: "Pip"}, "**Done** <3 see `x<y`"); err != nil {
		t.Fatal(err)
	}
	f.rejectHTML = true
	if err := a.Send(ctx, "42", channel.Persona{Name: "Pip"}, "odd **text"); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, "42", channel.Persona{Name: "Pip"}, strings.Repeat("word ", 2000)); err != nil {
		t.Fatal(err)
	}
	if got := f.sent[0]["text"]; got != "<b>Pip</b>\n<b>Done</b> &lt;3 see <code>x&lt;y</code>" {
		t.Fatalf("html: %q", got)
	}
	if f.sent[1]["parse_mode"] != nil || f.sent[1]["text"] != "Pip:\nodd **text" {
		t.Fatalf("plain fallback: %+v", f.sent[1])
	}
	if len(f.sent) != 2+3 || strings.HasPrefix(f.sent[3]["text"].(string), "<b>Pip</b>") {
		t.Fatalf("long text should be split with the name once: %d parts", len(f.sent)-2)
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	a := New(token, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.BaseURL = "http://127.0.0.1:1" // nothing listens
	err := a.Send(context.Background(), "42", channel.Persona{}, "hi")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the token or is nil: %v", err)
	}
	_, _, err = a.download(context.Background(), "x", 100)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("download error leaks the token: %v", err)
	}
	b, f := newTest(t)
	_ = f
	if _, _, err := b.download(context.Background(), "big", 100); err == nil || !strings.Contains(err.Error(), "20 MB") {
		t.Fatalf("too-big file: %v", err)
	}
}

func TestToHTML(t *testing.T) {
	for in, want := range map[string]string{
		"# Plan\n**a** & [docs](https://x.y/?a=1&b=2)": "<b>Plan</b>\n<b>a</b> &amp; <a href=\"https://x.y/?a=1&amp;b=2\">docs</a>",
		"```go\nif a < b {}\n```":                      "<pre>if a &lt; b {}</pre>",
		"[bad](javascript:alert(1))":                   "[bad](javascript:alert(1))",
	} {
		if got := toHTML(in); got != want {
			t.Errorf("toHTML(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSendKeepsCodeBlocksAcrossChunks(t *testing.T) {
	a, f := newTest(t)
	code := strings.Repeat("x := 1 // a line of code\n", 200) // ~5,000 bytes
	if err := a.Send(context.Background(), "42", channel.Persona{Name: "Pip"}, "Here:\n```\n"+code+"```\nThat's all."); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) < 2 {
		t.Fatalf("want the reply split, got %d part(s)", len(f.sent))
	}
	for i, m := range f.sent {
		html := m["text"].(string)
		if strings.Count(html, "<pre>") != strings.Count(html, "</pre>") || len(html) > 4096 {
			t.Errorf("part %d: unbalanced or too long (%d bytes)", i, len(html))
		}
		if i < len(f.sent)-1 && !strings.Contains(html, "<pre>") {
			t.Errorf("part %d lost the code formatting", i)
		}
	}
	last := f.sent[len(f.sent)-1]["text"].(string)
	if !strings.HasSuffix(last, "</pre>\nThat's all.") {
		t.Errorf("text after the block came out as code: %q", last[max(0, len(last)-60):])
	}
}

func TestAlbumIsOneMessage(t *testing.T) {
	a, f := newTest(t)
	a.albumWait, a.albumMaxWait = 150*time.Millisecond, time.Second
	photo := func(id string) []any { return []any{map[string]any{"file_id": id, "file_size": 100}} }
	f.updates = []map[string]any{
		msg(11, 42, "private", "", map[string]any{"media_group_id": "g1", "photo": photo("a")}),
		msg(10, 42, "private", "", map[string]any{"media_group_id": "g1", "caption": "which of these is ripe?", "photo": photo("b")}),
		msg(12, 42, "private", "", map[string]any{"media_group_id": "g1", "photo": photo("c")}),
		// Same group id in another chat: a separate album.
		msg(13, 43, "private", "", map[string]any{"media_group_id": "g1", "photo": photo("d")}),
		msg(14, 42, "private", "and a question on its own", nil),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan channel.Incoming, 10)
	go a.Run(ctx, func(_ context.Context, in channel.Incoming) { got <- in })

	seen := map[string]channel.Incoming{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 3 {
		select {
		case in := <-got:
			if _, dup := seen[in.MessageID]; dup {
				t.Fatalf("message %s delivered twice", in.MessageID)
			}
			seen[in.MessageID] = in
		case <-deadline:
			t.Fatalf("got only %v", seen)
		}
	}
	al, ok := seen["10"]
	if !ok || al.Text != "which of these is ripe?" || len(al.Attachments) != 3 || al.ChatID != "42" {
		t.Fatalf("album: %+v (all: %v)", al, seen)
	}
	if other := seen["13"]; len(other.Attachments) != 1 || other.ChatID != "43" {
		t.Fatalf("album in another chat: %+v", other)
	}
	if plain := seen["14"]; plain.Text != "and a question on its own" {
		t.Fatalf("plain message: %+v", plain)
	}
	select {
	case in := <-got:
		t.Fatalf("extra message: %+v", in)
	case <-time.After(300 * time.Millisecond):
	}
	a.albumsMu.Lock()
	defer a.albumsMu.Unlock()
	if len(a.albums) != 0 {
		t.Fatalf("albums left pending: %v", a.albums)
	}
}

// Parts that keep trickling in are still sent by albumMaxWait.
func TestAlbumMaxWait(t *testing.T) {
	a, _ := newTest(t)
	a.albumWait, a.albumMaxWait = 100*time.Millisecond, 250*time.Millisecond
	got := make(chan time.Time, 10)
	h := func(_ context.Context, in channel.Incoming) { got <- time.Now() }
	ctx := context.Background()
	start := time.Now()
	for i := int64(1); i <= 8; i++ {
		m := &message{MessageID: i, MediaGroupID: "g"}
		a.deliver(ctx, h, m, channel.Incoming{ChatID: "1", MessageID: strconv.FormatInt(i, 10)})
		time.Sleep(60 * time.Millisecond)
	}
	select {
	case at := <-got:
		if d := at.Sub(start); d > 400*time.Millisecond {
			t.Fatalf("first flush after %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("album never sent")
	}
}

func TestReactions(t *testing.T) {
	a, f := newTest(t)
	react := func(id int, user map[string]any, old, new []string) map[string]any {
		list := func(es []string) []any {
			var out []any
			for _, e := range es {
				out = append(out, map[string]any{"type": "emoji", "emoji": e})
			}
			return out
		}
		return map[string]any{"update_id": id, "message_reaction": map[string]any{"chat": map[string]any{"id": 42, "type": "private"},
			"message_id": 77, "user": user, "date": 1, "old_reaction": list(old), "new_reaction": list(new)}}
	}
	sam := map[string]any{"id": 42, "first_name": "Sam"}
	f.updates = []map[string]any{
		react(1, sam, nil, []string{"✍"}),
		react(2, sam, []string{"✍"}, []string{"✍", "🔥"}),                      // only 🔥 is new
		react(3, sam, []string{"🔥"}, nil),                                     // a removal
		react(4, map[string]any{"id": 5, "is_bot": true}, nil, []string{"✍"}), // a bot
	}
	got := make(chan channel.Reaction, 10)
	a.OnReaction(func(_ context.Context, r channel.Reaction) { got <- r })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(context.Context, channel.Incoming) {})
	var emojis []string
	deadline := time.After(5 * time.Second)
	for len(emojis) < 2 {
		select {
		case r := <-got:
			if r.ChatID != "42" || r.MessageID != "77" || r.UserID != "42" || !r.IsDM || r.ChatRef() != "telegram:42" {
				t.Fatalf("reaction: %+v", r)
			}
			emojis = append(emojis, r.Emoji)
		case <-deadline:
			t.Fatalf("reactions never arrived: %v", emojis)
		}
	}
	select {
	case r := <-got:
		t.Fatalf("unexpected reaction %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	sort.Strings(emojis) // reactions are handled concurrently, so they can arrive in either order
	if strings.Join(emojis, ",") != "✍,🔥" {
		t.Fatalf("reactions: %v", emojis)
	}
	f.mu.Lock()
	allowed := f.allowed
	f.mu.Unlock()
	if len(allowed) != 2 || allowed[1] != "message_reaction" {
		t.Fatalf("allowed_updates %v", allowed)
	}
}
