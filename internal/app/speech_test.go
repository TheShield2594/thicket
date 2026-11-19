package app

// Spoken replies end to end: a voice note to a sprig on Telegram (a fake
// Bot API) gets a voice note back in that sprig's voice (a fake speech
// server), after the text.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/channel/telegram"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

const tgToken = "123456:TEST-token-abcdefghijklmnopqrstuv"

// botAPI is a fake Telegram Bot API: it hands out queued updates and
// records what the bot posts, in order.
type botAPI struct {
	mu      sync.Mutex
	updates []map[string]any
	nextID  int
	posts   []map[string]string // method, chat_id, text / caption, audio
}

func (b *botAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/bot"+tgToken+"/") {
			w.Header().Set("Content-Type", "audio/ogg")
			w.Write([]byte("OggS fake opus"))
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+tgToken+"/")
		p := map[string]string{"method": method}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upload: %v", err)
			}
			for k, v := range r.MultipartForm.Value {
				p[k] = v[0]
			}
			for k, fs := range r.MultipartForm.File {
				f, _ := fs[0].Open()
				body, _ := io.ReadAll(f)
				f.Close()
				p[k] = string(body)
			}
		} else {
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			for k, v := range raw {
				p[k] = fmt.Sprint(v)
			}
		}
		ok := func(v any) { out, _ := json.Marshal(map[string]any{"ok": true, "result": v}); w.Write(out) }
		b.mu.Lock()
		defer b.mu.Unlock()
		switch method {
		case "getMe":
			ok(map[string]any{"id": 999, "is_bot": true, "first_name": "Thicket", "username": "thicket_bot"})
		case "deleteWebhook", "sendChatAction":
			ok(true)
		case "getUpdates":
			ups := b.updates
			b.updates = nil
			if len(ups) == 0 {
				b.mu.Unlock()
				time.Sleep(20 * time.Millisecond) // a long poll, briefly
				b.mu.Lock()
				ups = []map[string]any{}
			}
			ok(ups)
		case "getFile":
			ok(map[string]any{"file_path": "voice/file_1.oga"})
		case "sendMessage", "sendVoice", "sendAudio", "sendDocument":
			b.nextID++
			p["id"] = fmt.Sprint(1000 + b.nextID)
			b.posts = append(b.posts, p)
			ok(map[string]any{"message_id": 1000 + b.nextID})
		case "deleteMessage":
			b.posts = append(b.posts, p)
			ok(true)
		default:
			t.Errorf("unexpected Bot API method %s", method)
		}
	})
}

// voiceNote queues a voice message from user (a private chat).
func (b *botAPI) voiceNote(user int64, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.updates = append(b.updates, map[string]any{"update_id": b.nextID, "message": map[string]any{
		"message_id": b.nextID, "from": map[string]any{"id": user, "first_name": name},
		"chat":  map[string]any{"id": user, "type": "private"},
		"voice": map[string]any{"file_id": fmt.Sprint("v", b.nextID), "mime_type": "audio/ogg", "file_size": 14, "duration": 2},
	}})
}

func (b *botAPI) take() []map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.posts
	b.posts = nil
	return out
}

func (b *botAPI) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.posts)
}

// speechServer is a fake OpenAI-compatible speech server.
type speechServer struct {
	mu   sync.Mutex
	reqs []map[string]string
}

func (s *speechServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("speech request: %v", err)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "audio/ogg")
		fmt.Fprintf(w, "OggS voice=%s", req["voice"])
	})
}

func (s *speechServer) take() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

const pipSpeaks = `---
id: pip
name: Pip
role: Keeps the calendar and makes sure nothing is late.
personality:
  traits: [punctual, upbeat]
voice:
  style: [brisk and cheerful]
  sounds_like: nova
tools: [schedule]
---
Pip is small and tidy.
`

func setupTelegram(t *testing.T) (*App, *botAPI, *speechServer, *fakeModel, context.Context) {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("THICKET_DATA_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("THICKET_DATA_DIR") })
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.CharactersDir = filepath.Join(dir, "characters")
	cfg.DiscordOwnerID, cfg.TelegramOwnerID = "", ""

	bot := &botAPI{}
	botSrv := httptest.NewServer(bot.handler(t))
	t.Cleanup(botSrv.Close)
	tg := telegram.New(tgToken, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tg.BaseURL = botSrv.URL
	speech := &speechServer{}
	speechSrv := httptest.NewServer(speech.handler(t))
	t.Cleanup(speechSrv.Close)

	fm := &fakeModel{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, "test", Options{
		Model: fm, KeyCheck: func(context.Context, string) error { return nil },
		Channels: func(*slog.Logger) []channel.Channel { return []channel.Channel{tg} },
		LogOut:   io.Discard, NoHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := os.WriteFile(filepath.Join(cfg.CharactersDir, "pip.md"), []byte(pipSpeaks), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	if err := a.Secrets.Put(ctx, secrets.OpenRouterKey, "sk-or-test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "speech_url", speechSrv.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "speech_model", "kokoro"); err != nil {
		t.Fatal(err)
	}
	go tg.Run(ctx, a.Engine.HandleIncoming)
	waitFor(t, "telegram", tg.Connected)
	return a, bot, speech, fm, ctx
}

// tgText is a typed Telegram DM, handled synchronously.
func tgText(user int64, text string) channel.Incoming {
	id := fmt.Sprint(user)
	return channel.Incoming{Channel: "telegram", ChatID: id, MessageID: fmt.Sprint(time.Now().UnixNano()), UserID: id, UserName: "Sam", Text: text, IsDM: true}
}

func TestVoiceNoteGetsVoiceNoteBack(t *testing.T) {
	a, bot, speech, fm, ctx := setupTelegram(t)

	// A voice note to the host: the text, then a voice note in the
	// default voice.
	bot.voiceNote(42, "Sam")
	waitFor(t, "a voice note back", func() bool { return bot.count() >= 2 })
	posts := bot.take()
	if len(posts) != 2 || posts[0]["method"] != "sendMessage" || posts[1]["method"] != "sendVoice" {
		t.Fatalf("want the text then a voice note: %v", posts)
	}
	if !strings.Contains(posts[0]["text"], "Juni") || posts[1]["chat_id"] != "42" || posts[1]["caption"] != "<b>Juni</b>" ||
		posts[1]["voice"] != "OggS voice=alloy" {
		t.Fatalf("host's reply: %v", posts)
	}
	reqs := speech.take()
	if len(reqs) != 1 || reqs[0]["model"] != "kokoro" || reqs[0]["voice"] != "alloy" || reqs[0]["response_format"] != "opus" ||
		reqs[0]["input"] != "Hi! I'm Juni. Lovely to meet you." {
		t.Fatalf("speech request: %v", reqs)
	}

	// Typed messages get typed answers (Pip speaks when spoken to).
	a.Engine.HandleIncoming(ctx, tgText(42, "Pip, hi"))
	if posts := bot.take(); len(posts) != 1 || posts[0]["method"] != "sendMessage" || !strings.Contains(posts[0]["text"], "Pip here") {
		t.Fatalf("typed message: %v", posts)
	}

	// A voice note to Pip comes back in Pip's voice.
	bot.voiceNote(42, "Sam")
	waitFor(t, "Pip's voice note", func() bool { return bot.count() >= 2 })
	posts = bot.take()
	if len(posts) != 2 || posts[0]["method"] != "sendMessage" || !strings.Contains(posts[0]["text"], "Pip here") ||
		posts[1]["method"] != "sendVoice" || posts[1]["caption"] != "<b>Pip</b>" || posts[1]["voice"] != "OggS voice=nova" {
		t.Fatalf("Pip's reply: %v", posts)
	}
	if reqs := speech.take(); len(reqs) != 1 || reqs[0]["voice"] != "nova" || reqs[0]["input"] != "Pip here! I'll sort your calendar." {
		t.Fatalf("Pip's speech: %v", reqs)
	}
	// "delete that" takes the spoken copy with the text.
	a.Engine.HandleIncoming(ctx, tgText(42, "delete that"))
	dels := bot.take()
	if len(dels) != 2 || dels[0]["method"] != "deleteMessage" || dels[0]["message_id"] != posts[0]["id"] ||
		dels[1]["method"] != "deleteMessage" || dels[1]["message_id"] != posts[1]["id"] {
		t.Fatalf("delete that: %v (posted %v)", dels, posts)
	}

	// The person turns Pip's voice off: text only, even for a voice note.
	a.Engine.HandleIncoming(ctx, tgText(42, "Pip, voice off"))
	if posts := bot.take(); len(posts) != 1 || !strings.Contains(posts[0]["text"], "only write") {
		t.Fatalf("voice off: %v", posts)
	}
	owner, err := a.DB.UserByExternal(ctx, "telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	if mode, _ := a.DB.SpeechMode(ctx, owner.ID, "pip"); mode != store.SpeechOff {
		t.Fatalf("pip's mode = %q", mode)
	}
	bot.voiceNote(42, "Sam")
	waitFor(t, "Pip's text reply", func() bool { return bot.count() >= 1 })
	// The chat's turns run one at a time: once this returns, so has Pip's.
	a.Engine.HandleIncoming(ctx, tgText(42, "Pip, voice"))
	posts = bot.take()
	if len(posts) != 2 || posts[0]["method"] != "sendMessage" || posts[1]["method"] != "sendMessage" ||
		!strings.Contains(posts[1]["text"], "text only") {
		t.Fatalf("voice off should mean text only: %v", posts)
	}
	if reqs := speech.take(); len(reqs) != 0 {
		t.Fatalf("spoke with voice off: %v", reqs)
	}

	// Brief: a long reply is spoken as a short version; the text is whole.
	long := "Here is the whole plan for the week, in detail. " + strings.Repeat("First we tidy the shed and sort the seeds by season. ", 12)
	fm.mu.Lock()
	fm.say = long
	fm.mu.Unlock()
	a.Engine.HandleIncoming(ctx, tgText(42, "Juni, voice brief"))
	a.Engine.HandleIncoming(ctx, tgText(42, "Juni, are you there?")) // Juni is the one talking now
	bot.take()
	bot.voiceNote(42, "Sam")
	waitFor(t, "Juni's brief voice note", func() bool { return bot.count() >= 2 })
	posts = bot.take()
	if len(posts) != 2 || posts[0]["text"] != "<b>Juni</b>\n"+strings.TrimSpace(long) || posts[1]["method"] != "sendVoice" {
		t.Fatalf("brief: %v", posts)
	}
	reqs = speech.take()
	if len(reqs) != 1 || len(reqs[0]["input"]) >= len(long)/2 || reqs[0]["input"] == "" {
		t.Fatalf("brief should speak a short version: %v", reqs)
	}
	var briefCalls int
	fm.mu.Lock()
	for _, r := range fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You turn a chat reply into") {
			briefCalls++
		}
	}
	fm.mu.Unlock()
	if briefCalls != 1 {
		t.Fatalf("brief versions made: %d", briefCalls)
	}

	// Speech is metered like any other call.
	var n int
	a.DB.DB().QueryRow("SELECT COUNT(*) FROM usage WHERE purpose = 'speech' AND model = 'kokoro'").Scan(&n)
	if n != 3 {
		t.Fatalf("speech usage rows = %d, want 3", n)
	}
}

func TestKidSprigSpeaksOnlyWithParentsSayso(t *testing.T) {
	a, bot, speech, _, ctx := setupTelegram(t)
	a.Engine.HandleIncoming(ctx, tgText(42, "hi")) // the owner
	bot.take()
	owner, err := a.DB.UserByExternal(ctx, "telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	kid, err := a.DB.CreateUser(ctx, "telegram", "77", "Max", store.RoleKid)
	if err != nil {
		t.Fatal(err)
	}
	c, err := character.Parse([]byte(fmt.Sprintf(`---
id: bramble
name: Bramble
role: A friendly companion for Max.
household: kid
kids: [%d]
parents: [%d]
personality:
  traits: [playful]
voice:
  style: [short and kind]
  speaks: always
---
Bramble likes games.
`, kid.ID, owner.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Chars.Write(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Before any sprig has talked in the kid's chat, "voice off" is for
	// the sprig that would answer: the kid's own, not the host.
	a.Engine.HandleIncoming(ctx, tgText(77, "voice off"))
	if posts := bot.take(); len(posts) != 1 || !strings.Contains(posts[0]["text"], "only write") || strings.Contains(posts[0]["text"], "Juni") {
		t.Fatalf("kid's voice off: %v", posts)
	}
	if mode, _ := a.DB.SpeechMode(ctx, kid.ID, "bramble"); mode != store.SpeechOff {
		t.Fatalf("bramble's mode = %q", mode)
	}
	if host, _ := a.Chars.Host(); host != nil {
		if mode, _ := a.DB.SpeechMode(ctx, kid.ID, host.ID); mode != store.SpeechFull {
			t.Fatalf("voice off went to the host: %q", mode)
		}
	}
	a.Engine.HandleIncoming(ctx, tgText(77, "voice full"))
	bot.take()

	a.Engine.HandleIncoming(ctx, tgText(77, "hello!"))
	if posts := bot.take(); len(posts) != 1 || posts[0]["method"] != "sendMessage" {
		t.Fatalf("kid sprig spoke without a parent's say-so: %v", posts)
	}
	a.Engine.HandleIncoming(ctx, tgText(77, "voice"))
	if posts := bot.take(); len(posts) != 1 || !strings.Contains(posts[0]["text"], "grown-up") {
		t.Fatalf("voice status for a kid: %v", posts)
	}
	if _, err := a.Engine.KidAccess(ctx, owner, "bramble", nil, nil, []string{character.AllowVoice}, nil); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, tgText(77, "hello again!"))
	if posts := bot.take(); len(posts) != 2 || posts[1]["method"] != "sendVoice" || posts[1]["caption"] != "<b>Bramble</b>" {
		t.Fatalf("kid sprig allowed to speak: %v", posts)
	}
	if reqs := speech.take(); len(reqs) != 1 {
		t.Fatalf("speech requests: %v", reqs)
	}
}
