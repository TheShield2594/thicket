package app

// Local speech end to end (docs/voice.md): with the voice plugin running
// (a fake one here, speaking its real protocol) and no OpenRouter key, a
// voice message on Telegram and in the web app is transcribed by the
// plugin and answered out loud by it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/plugin"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// fakeVoice is the voice plugin: /health, MCP, and the two OpenAI-style
// audio endpoints, all but /health behind the token in tokenFile.
type fakeVoice struct {
	t         *testing.T
	tokenFile string
	srv       *httptest.Server
	heard     string // what every recording "says" (unless it is the doctor's)
	failSTT   bool

	mu     sync.Mutex
	stt    []map[string]string // file, filename, model, auth
	speech []map[string]any
}

func newFakeVoice(t *testing.T) *fakeVoice {
	t.Helper()
	v := &fakeVoice{t: t, tokenFile: filepath.Join(t.TempDir(), "voice-auth", "token"), heard: "Good morning, how are you?"}
	v.srv = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeVoice) authorized(r *http.Request) bool {
	tok, _ := os.ReadFile(v.tokenFile)
	want := strings.TrimSpace(string(tok))
	return want != "" && r.Header.Get("Authorization") == "Bearer "+want
}

func (v *fakeVoice) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		fmt.Fprint(w, `{"status":"ready"}`)
		return
	}
	if !v.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"unauthorized"}}`)
		return
	}
	switch r.URL.Path {
	case "/mcp":
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"thicket-voice","version":"0.1.0"}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"list_voices","description":"voices","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}`, *req.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"Local voices: en_US-lessac-medium."}]}}`, *req.ID)
		}
	case "/v1/audio/transcriptions":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			v.t.Errorf("transcription form: %v", err)
			return
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			v.t.Errorf("transcription file: %v", err)
			return
		}
		audio, _ := io.ReadAll(f)
		v.mu.Lock()
		v.stt = append(v.stt, map[string]string{"file": string(audio), "filename": h.Filename, "model": r.FormValue("model")})
		fail := v.failSTT
		v.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"whisper-cli failed: exit status 3"}}`)
			return
		}
		text := v.heard
		if said, ok := strings.CutPrefix(string(audio), "RIFF "); ok {
			text = said // the doctor's round trip hears what was said
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": text})
	case "/v1/audio/speech":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		v.mu.Lock()
		v.speech = append(v.speech, req)
		v.mu.Unlock()
		if req["response_format"] == "wav" {
			w.Header().Set("Content-Type", "audio/wav")
			fmt.Fprintf(w, "RIFF %s", req["input"])
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		fmt.Fprintf(w, "OggS piper voice=%s", req["voice"])
	default:
		http.NotFound(w, r)
	}
}

func (v *fakeVoice) take() (stt []map[string]string, speech []map[string]any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	stt, speech = v.stt, v.speech
	v.stt, v.speech = nil, nil
	return
}

// startVoice points Thicket at the fake plugin and sets it up the way a
// start with THICKET_VOICE_URL set does.
func startVoice(t *testing.T, a *App, ctx context.Context) *fakeVoice {
	t.Helper()
	v := newFakeVoice(t)
	a.Cfg.SetPluginEnv("THICKET_VOICE_URL", v.srv.URL)
	a.Cfg.SetPluginEnv("THICKET_VOICE_TOKEN_FILE", v.tokenFile)
	if !a.ensurePlugins(ctx) {
		t.Fatal("the voice plugin wasn't set up")
	}
	return v
}

// localModel points Thicket at a model server of its own (the fake model
// answers): no OpenRouter, no key.
func localModel(t *testing.T, a *App, ctx context.Context) {
	t.Helper()
	for k, v := range map[string]string{"model_url": "http://127.0.0.1:1/v1", "model": "qwen2.5:14b", "cheap_model": "qwen2.5:14b", "fallback_model": "qwen2.5:14b"} {
		if err := a.Settings.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if a.Secrets.Has(ctx, secrets.OpenRouterKey) {
		t.Fatal("an OpenRouter key is set")
	}
}

func lastUserContent(fm *fakeModel) string {
	req := lastChatReq(fm)
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func asksModelToTranscribe(fm *fakeModel) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, r := range fm.reqs {
		if len(r.Messages) > 0 && strings.HasPrefix(r.Messages[0].Content, "You transcribe audio") {
			return true
		}
	}
	return false
}

func TestTelegramVoiceNoteWithLocalVoiceAndNoOpenRouter(t *testing.T) {
	a, bot, fm, ctx, start := setupTelegramBare(t)
	localModel(t, a, ctx)
	v := startVoice(t, a, ctx)
	if !a.Settings.Bool(ctx, "local_voice") {
		t.Fatal("a new install should use local voice by default")
	}
	start()

	bot.voiceNote(42, "Sam")
	waitFor(t, "a voice note back", func() bool { return bot.count() >= 2 })
	posts := bot.take()
	if len(posts) != 2 || posts[0]["method"] != "sendMessage" || posts[1]["method"] != "sendVoice" || posts[1]["voice"] != "OggS piper voice=alloy" {
		t.Fatalf("want the text then the plugin's voice note: %v", posts)
	}
	stt, speech := v.take()
	if len(stt) != 1 || stt[0]["file"] != "OggS fake opus" || stt[0]["filename"] != "voice.ogg" || stt[0]["model"] != "whisper-1" {
		t.Fatalf("transcription request: %v", stt)
	}
	if len(speech) != 1 || speech[0]["response_format"] != "opus" || speech[0]["input"] != "Hi! I'm Juni. Lovely to meet you." {
		t.Fatalf("speech request: %v", speech)
	}
	if got := lastUserContent(fm); !strings.Contains(got, "transcribed:]\nGood morning, how are you?") {
		t.Fatalf("the sprig didn't get the words:\n%s", got)
	}
	if asksModelToTranscribe(fm) {
		t.Fatal("the model server was asked to transcribe")
	}
	// Both are metered, at no cost.
	var n int
	var cost float64
	a.DB.DB().QueryRow("SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM usage WHERE purpose IN ('transcribe', 'speech')").Scan(&n, &cost)
	if n != 2 || cost != 0 {
		t.Fatalf("voice usage rows %d, cost %v", n, cost)
	}

	// A failing plugin: the sprig is told, and asks the person to type.
	v.mu.Lock()
	v.failSTT = true
	v.mu.Unlock()
	bot.voiceNote(42, "Sam")
	waitFor(t, "a reply", func() bool { return bot.count() >= 1 })
	if got := lastUserContent(fm); !strings.Contains(got, "could not be transcribed: ") || !strings.Contains(got, "whisper-cli failed") {
		t.Fatalf("failure note:\n%s", got)
	}
}

func TestWebVoiceMessageWithLocalVoiceAndNoOpenRouter(t *testing.T) {
	a, _, fm, ctx := setup(t)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pipSpeaks), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	localModel(t, a, ctx)
	v := startVoice(t, a, ctx)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	w := &webClient{t: t, base: srv.URL, c: &http.Client{Jar: jar}}
	token, _ := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if code, out := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	owner, _ := a.DB.Owner(ctx)
	chat := fmt.Sprintf("%d.pip", owner.ID)

	if code, out := w.postFiles("/api/chats/"+chat+"/messages", "", upload{"voice message.webm", "audio/webm", "webm bytes"}); code != 200 {
		t.Fatalf("upload: %d %v", code, out)
	}
	var msgs []store.WebMessage
	waitFor(t, "a spoken reply", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, chat, 0, 10)
		return len(msgs) == 2 && msgs[1].Audio
	})
	stt, speech := v.take()
	if len(stt) != 1 || stt[0]["file"] != "webm bytes" || stt[0]["filename"] != "voice.webm" {
		t.Fatalf("transcription: %v", stt)
	}
	if len(speech) != 1 || speech[0]["voice"] != "nova" || speech[0]["response_format"] != "mp3" {
		t.Fatalf("speech: %v", speech)
	}
	if got := lastUserContent(fm); !strings.Contains(got, "transcribed:]\nGood morning, how are you?") || asksModelToTranscribe(fm) {
		t.Fatalf("the sprig didn't get the words from the plugin:\n%s", got)
	}
	res, err := w.c.Get(srv.URL + "/api/chats/" + chat + "/messages/" + strconv.FormatInt(msgs[1].ID, 10) + "/audio")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "OggS piper voice=nova" {
		t.Fatalf("audio: %d %q", res.StatusCode, body)
	}
}

// An install upgraded from before local voice keeps what it did; one
// setting switches it over, and explicit addresses always win.
func TestLocalVoiceDefaults(t *testing.T) {
	a, _, _, ctx := setup(t)
	v := startVoice(t, a, ctx)
	if got := a.transcribeURL(ctx); got != v.srv.URL+"/v1" {
		t.Fatalf("new install, transcription at %q", got)
	}
	if got := a.speechURL(ctx); got != v.srv.URL+"/v1" {
		t.Fatalf("new install, speech at %q", got)
	}
	// The plugin's address gets the plugin's token, nothing else does.
	tok, _ := os.ReadFile(v.tokenFile)
	key := a.voiceKey(func(context.Context, string) (string, error) { return "model-key", nil })
	if k, _ := key(ctx, v.srv.URL+"/v1"); k != strings.TrimSpace(string(tok)) || k == "" {
		t.Fatalf("plugin key %q", k)
	}
	if k, _ := key(ctx, "http://192.168.1.20:8880/v1"); k != "model-key" {
		t.Fatalf("other server's key %q", k)
	}

	// The same database, as if upgraded: people were already here when
	// this version first started.
	if _, err := a.DB.CreateUser(ctx, "telegram", "42", "Sam", store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.DeleteSetting(ctx, localVoiceDecidedKey); err != nil {
		t.Fatal(err)
	}
	a.decideLocalVoice(ctx)
	if a.Settings.Bool(ctx, "local_voice") || a.transcribeURL(ctx) != "" || a.speechURL(ctx) != llm.DefaultURL {
		t.Fatalf("existing install changed: local_voice=%v stt=%q speech=%q", a.Settings.Bool(ctx, "local_voice"), a.transcribeURL(ctx), a.speechURL(ctx))
	}
	// Decided once: a later start leaves the owner's choice alone.
	a.Settings.Set(ctx, "local_voice", "true")
	a.decideLocalVoice(ctx)
	if !a.Settings.Bool(ctx, "local_voice") || a.transcribeURL(ctx) != v.srv.URL+"/v1" {
		t.Fatal("turning local_voice on didn't stick")
	}
	// Explicit addresses win over the plugin.
	a.Settings.Set(ctx, "transcribe_url", "http://192.168.1.20:9000/v1")
	a.Settings.Set(ctx, "speech_url", "http://192.168.1.20:8880/v1")
	if a.transcribeURL(ctx) != "http://192.168.1.20:9000/v1" || a.speechURL(ctx) != "http://192.168.1.20:8880/v1" {
		t.Fatalf("explicit: %q %q", a.transcribeURL(ctx), a.speechURL(ctx))
	}
	a.Settings.Set(ctx, "transcribe_url", "")
	a.Settings.Set(ctx, "speech_url", "")

	// The plugin turned off: its integration is off, and nothing goes there.
	a.Cfg.SetPluginEnv("THICKET_VOICE_URL", "off")
	a.ensurePlugins(ctx)
	if a.transcribeURL(ctx) != "" || a.speechURL(ctx) != llm.DefaultURL {
		t.Fatalf("plugin off: %q %q", a.transcribeURL(ctx), a.speechURL(ctx))
	}
}

// Without the plugin and without OpenRouter, the sprig is told why a
// voice message wasn't heard.
func TestVoiceMessageWithNothingToHearIt(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	a.Engine.FetchAttachment = fakeFetch
	localModel(t, a, ctx)
	in := dm("")
	in.Attachments = []channel.Attachment{{Name: "voice message.ogg", URL: "https://cdn.example/voice.ogg", ContentType: "audio/ogg", Size: 14}}
	a.Engine.HandleIncoming(ctx, in)
	fc.take()
	if got := lastUserContent(fm); !strings.Contains(got, "was not transcribed: that needs a speech-to-text server (the voice plugin, or the transcribe_url setting)") {
		t.Fatalf("note:\n%s", got)
	}
}

func TestDoctorVoiceRoundTrip(t *testing.T) {
	a, _, _, ctx := setup(t)
	v := startVoice(t, a, ctx)
	c := doctorCheck(t, a, plugin.Builtin(), "plugin.voice")
	if !c.OK || !strings.Contains(c.Detail, "token accepted") || !strings.Contains(c.Detail, "round trip in") || strings.Contains(c.Detail, "heard") {
		t.Fatalf("doctor: %+v", c)
	}
	stt, speech := v.take()
	if len(speech) != 1 || speech[0]["response_format"] != "wav" || len(stt) != 1 || stt[0]["file"] != "RIFF Testing, one two three." {
		t.Fatalf("round trip: %v %v", speech, stt)
	}
	v.mu.Lock()
	v.failSTT = true
	v.mu.Unlock()
	if c := doctorCheck(t, a, plugin.Builtin(), "plugin.voice"); c.OK || !strings.Contains(c.Detail, "transcription") || !strings.Contains(c.Detail, "whisper-cli failed") {
		t.Fatalf("doctor, failing: %+v", c)
	}
}
