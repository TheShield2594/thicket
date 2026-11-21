package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-0123456789abcdefghijklmnop"

// Fake programs. Each writes what it was given into its output, so the
// tests can see the whole pipeline; FAKE_LOG collects their arguments.
const fakeFFmpeg = `#!/bin/sh
in=""; fmt=wav; prev=""; out=""
for a in "$@"; do
  [ "$prev" = "-i" ] && in="$a"
  [ "$prev" = "-f" ] && fmt="$a"
  prev="$a"; out="$a"
done
echo "ffmpeg $*" >> "$FAKE_LOG"
if grep -q unreadable "$in"; then echo "Invalid data found when processing input" >&2; exit 1; fi
printf '%s:' "$fmt" > "$out"; cat "$in" >> "$out"
`

const fakeWhisper = `#!/bin/sh
wav=""; of=""; lang=""; prev=""
for a in "$@"; do
  [ "$prev" = "-f" ] && wav="$a"
  [ "$prev" = "-of" ] && of="$a"
  [ "$prev" = "-l" ] && lang="$a"
  prev="$a"
done
echo "whisper $*" >> "$FAKE_LOG"
if grep -q crash "$wav"; then echo "whisper_init: failed to load model" >&2; exit 3; fi
printf ' heard %s:\n  %s \n' "$lang" "$(cat "$wav")" > "$of.txt"
`

const fakePiper = `#!/bin/sh
out=""; model=""; ls=""; prev=""
for a in "$@"; do
  [ "$prev" = "--output_file" ] && out="$a"
  [ "$prev" = "--model" ] && model="$a"
  [ "$prev" = "--length_scale" ] && ls="$a"
  prev="$a"
done
echo "piper $* LD=$LD_LIBRARY_PATH" >> "$FAKE_LOG"
text=$(cat)
printf 'RIFF voice=%s ls=%s text=%s' "$(basename "$model" .onnx)" "$ls" "$text" > "$out"
`

type harness struct {
	t      *testing.T
	s      *server
	srv    *httptest.Server
	log    string
	models string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(filepath.Join(bin, "piper"), 0o755)
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "ffmpeg"), fakeFFmpeg, 0o755)
	write(filepath.Join(bin, "whisper-cli"), fakeWhisper, 0o755)
	write(filepath.Join(bin, "piper", "piper"), fakePiper, 0o755)
	tokenFile := filepath.Join(dir, "auth", "token")
	os.MkdirAll(filepath.Dir(tokenFile), 0o755)
	write(tokenFile, testToken+"\n", 0o640)
	logFile := filepath.Join(dir, "calls.log")
	t.Setenv("FAKE_LOG", logFile)

	models := filepath.Join(dir, "models")
	env := map[string]string{
		"VOICE_TOKEN_FILE": tokenFile, "VOICE_MODELS_DIR": models,
		"VOICE_FFMPEG_BIN": filepath.Join(bin, "ffmpeg"), "VOICE_WHISPER_BIN": filepath.Join(bin, "whisper-cli"),
		"VOICE_PIPER_BIN": filepath.Join(bin, "piper", "piper"), "VOICE_THREADS": "2", "VOICE_TIMEOUT_SECONDS": "10",
	}
	cfg := configFromEnv(func(k string) string { return env[k] })
	// The models are "downloaded" already.
	for _, m := range cfg.Models {
		os.MkdirAll(filepath.Dir(m.Path), 0o755)
		write(m.Path, "model", 0o644)
	}
	s := newServer(cfg)
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, s: s, srv: srv, log: logFile, models: models}
}

func (h *harness) calls() string {
	b, _ := os.ReadFile(h.log)
	os.Remove(h.log)
	return string(b)
}

func (h *harness) do(req *http.Request, token string) (int, string, http.Header) {
	h.t.Helper()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (h *harness) transcribe(token string, audio []byte, fields map[string]string) (int, string) {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if audio != nil {
		fw, _ := mw.CreateFormFile("file", "voice.ogg")
		fw.Write(audio)
	}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	code, out, _ := h.do(req, token)
	return code, out
}

func (h *harness) speak(token string, req map[string]any) (int, string, string) {
	h.t.Helper()
	b, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/audio/speech", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	code, out, hdr := h.do(r, token)
	return code, out, hdr.Get("Content-Type")
}

func TestTokenAndReadiness(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"", "wrong-token-0123456789abcdefghijklmnop"} {
		if code, _ := h.transcribe(tok, []byte("OggS"), nil); code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, code)
		}
		if code, _, _ := h.speak(tok, map[string]any{"input": "hi"}); code != http.StatusUnauthorized {
			t.Fatalf("speech, token %q: %d", tok, code)
		}
	}
	// /health needs no token and says it is still starting.
	code, out, _ := h.do(mustReq(http.MethodGet, h.srv.URL+"/health"), "")
	if code != 200 || !strings.Contains(out, `"status":"starting"`) {
		t.Fatalf("health: %d %s", code, out)
	}
	if code, out := h.transcribe(testToken, []byte("OggS"), nil); code != http.StatusServiceUnavailable || !strings.Contains(out, "fetching its models") {
		t.Fatalf("not ready: %d %s", code, out)
	}
	// No token file: nothing is allowed.
	os.Remove(h.s.cfg.TokenFile)
	h.s.setStatus("ready", "")
	if code, _ := h.transcribe(testToken, []byte("OggS"), nil); code != http.StatusUnauthorized {
		t.Fatalf("no token file: %d", code)
	}
}

func mustReq(method, url string) *http.Request {
	r, _ := http.NewRequest(method, url, nil)
	return r
}

func TestTranscriptions(t *testing.T) {
	h := newHarness(t)
	h.s.setStatus("ready", "")

	code, out := h.transcribe(testToken, []byte("OggS hello there"), map[string]string{"model": "whisper-1", "language": "en"})
	if code != 200 {
		t.Fatalf("transcribe: %d %s", code, out)
	}
	var got struct{ Text string }
	json.Unmarshal([]byte(out), &got)
	if got.Text != "heard en: wav:OggS hello there" {
		t.Fatalf("text: %q", got.Text)
	}
	calls := h.calls()
	for _, want := range []string{"-ar 16000 -ac 1 -c:a pcm_s16le", "-m " + filepath.Join(h.models, "whisper", "ggml-base-q5_1.bin"), "-l en -t 2 -nt -np -otxt"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls missing %q:\n%s", want, calls)
		}
	}
	// No language: the configured one (auto). Plain text when asked.
	code, out = h.transcribe(testToken, []byte("OggS hi"), map[string]string{"response_format": "text"})
	if code != 200 || out != "heard auto: wav:OggS hi\n" {
		t.Fatalf("text format: %d %q", code, out)
	}
	for name, c := range map[string]struct {
		audio  []byte
		fields map[string]string
		code   int
		msg    string
	}{
		"no file":       {nil, nil, 400, "no recording"},
		"empty file":    {[]byte{}, nil, 400, "empty"},
		"bad language":  {[]byte("x"), map[string]string{"language": "../etc"}, 400, "language"},
		"srt":           {[]byte("x"), map[string]string{"response_format": "srt"}, 400, "response_format"},
		"unreadable":    {[]byte("unreadable"), nil, 500, "couldn't read the recording"},
		"whisper fails": {[]byte("crash"), nil, 500, "failed to load model"},
	} {
		if code, out := h.transcribe(testToken, c.audio, c.fields); code != c.code || !strings.Contains(out, c.msg) {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	// Nothing is left behind in the temporary folder.
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "voice-in-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestSpeech(t *testing.T) {
	h := newHarness(t)
	h.s.setStatus("ready", "")

	// OpenAI's voice names get the default voice; Telegram's opus is
	// converted by ffmpeg.
	code, out, ct := h.speak(testToken, map[string]any{"model": "openai/gpt-4o-mini-tts", "input": "Hi! I'm Juni.\nLovely to meet you.", "voice": "alloy", "response_format": "opus"})
	if code != 200 || ct != "audio/ogg" || out != "ogg:RIFF voice=en_US-lessac-medium ls= text=Hi! I'm Juni. Lovely to meet you." {
		t.Fatalf("speech: %d %q %q", code, ct, out)
	}
	calls := h.calls()
	if !strings.Contains(calls, "LD="+filepath.Dir(h.s.cfg.Piper)) || !strings.Contains(calls, "-c:a libopus") {
		t.Fatalf("calls:\n%s", calls)
	}
	// Another voice in the folder, at double speed, as WAV: no ffmpeg.
	alan := filepath.Join(h.models, "piper", "en_GB-alan-low.onnx")
	os.WriteFile(alan, []byte("m"), 0o644)
	os.WriteFile(alan+".json", []byte("{}"), 0o644)
	code, out, ct = h.speak(testToken, map[string]any{"input": "Cheerio.", "voice": "en_GB-alan-low", "response_format": "wav", "speed": 2})
	if code != 200 || ct != "audio/wav" || out != "RIFF voice=en_GB-alan-low ls=0.500 text=Cheerio." {
		t.Fatalf("wav: %d %q %q", code, ct, out)
	}
	if calls := h.calls(); strings.Contains(calls, "ffmpeg") {
		t.Fatalf("wav needs no conversion:\n%s", calls)
	}
	// The default format is mp3; a path is never a voice.
	code, out, ct = h.speak(testToken, map[string]any{"input": "Hello.", "voice": "../../etc/passwd"})
	if code != 200 || ct != "audio/mpeg" || !strings.HasPrefix(out, "mp3:RIFF voice=en_US-lessac-medium") {
		t.Fatalf("mp3: %d %q %q", code, ct, out)
	}
	for name, req := range map[string]map[string]any{
		"empty":      {"input": "  "},
		"too long":   {"input": strings.Repeat("a", maxInputChars+1)},
		"bad format": {"input": "x", "response_format": "midi"},
		"bad speed":  {"input": "x", "speed": 9},
	} {
		if code, out, _ := h.speak(testToken, req); code != 400 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
}

func TestMCP(t *testing.T) {
	h := newHarness(t)
	call := func(body string) map[string]any {
		t.Helper()
		r, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp", strings.NewReader(body))
		code, out, _ := h.do(r, testToken)
		if code != 200 {
			t.Fatalf("%s: %d %s", body, code, out)
		}
		var resp map[string]any
		json.Unmarshal([]byte(out), &resp)
		return resp
	}
	init := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["version"] != version {
		t.Fatalf("initialize: %v", init)
	}
	list := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("tools: %v", list)
	}
	res := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_voices","arguments":{}}}`)
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "Local voices: en_US-lessac-medium. Default: en_US-lessac-medium.") {
		t.Fatalf("list_voices: %s", b)
	}
	r, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if code, _, _ := h.do(r, testToken); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	r, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if code, _, _ := h.do(r, ""); code != http.StatusUnauthorized {
		t.Fatalf("mcp without a token: %d", code)
	}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestModelDownloadsAreChecked(t *testing.T) {
	files := map[string][]byte{"/model.bin": []byte("the real model"), "/voice.onnx": []byte("onnx"), "/voice.onnx.json": []byte("{}")}
	var mu sync.Mutex
	hits := 0
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		b, ok := files[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer src.Close()
	dir := t.TempDir()
	m := modelFile{Path: filepath.Join(dir, "whisper", "model.bin"), URL: src.URL + "/model.bin", SHA256: sum(files["/model.bin"])}
	ctx := context.Background()
	if err := m.ensure(ctx, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.Path); string(b) != "the real model" {
		t.Fatalf("model: %q", b)
	}
	// Present and matching: not fetched again.
	mu.Lock()
	hits = 0
	mu.Unlock()
	if err := m.ensure(ctx, http.DefaultClient); err != nil || hits != 0 {
		t.Fatalf("refetched: %v %d", err, hits)
	}
	// Changed on disk: fetched again.
	os.WriteFile(m.Path, []byte("tampered"), 0o644)
	if err := m.ensure(ctx, http.DefaultClient); err != nil || hits != 1 {
		t.Fatalf("tampered: %v %d", err, hits)
	}
	// The wrong checksum: refused, nothing kept.
	bad := modelFile{Path: filepath.Join(dir, "whisper", "other.bin"), URL: src.URL + "/model.bin", SHA256: sum([]byte("something else"))}
	if err := bad.ensure(ctx, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "whisper", "*")); len(left) != 1 {
		t.Fatalf("a bad download was kept: %v", left)
	}
	if err := (modelFile{Path: filepath.Join(dir, "x"), URL: src.URL + "/missing"}).ensure(ctx, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
	if err := (modelFile{Path: filepath.Join(dir, "y")}).ensure(ctx, http.DefaultClient); err == nil {
		t.Fatal("a missing file with no address")
	}

	// prepare: an unreachable file keeps it starting, and once it can be
	// fetched the plugin is ready.
	s := newServer(config{Jobs: 1, ModelsDir: dir, Models: []modelFile{
		{Path: filepath.Join(dir, "piper", "v.onnx"), URL: src.URL + "/voice.onnx", SHA256: sum(files["/voice.onnx"])},
		{Path: filepath.Join(dir, "piper", "v.onnx.json"), URL: src.URL + "/late.json", SHA256: sum(files["/voice.onnx.json"])},
	}})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { s.prepare(ctx, 10*time.Millisecond); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if st, detail := s.state(); st == "error" && strings.Contains(detail, "404") {
			break
		}
		if time.Now().After(deadline) {
			st, detail := s.state()
			t.Fatalf("state: %s %s", st, detail)
		}
	}
	mu.Lock()
	files["/late.json"] = files["/voice.onnx.json"]
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	if st, _ := s.state(); st != "ready" {
		t.Fatalf("state: %s", st)
	}
}

func TestConfig(t *testing.T) {
	c := configFromEnv(func(string) string { return "" })
	if c.Addr != ":7071" || c.TokenFile != "/plugin-auth/token" || c.Voice != defaultVoice || len(c.Models) != 3 {
		t.Fatalf("defaults: %+v", c)
	}
	for _, m := range c.Models {
		if m.URL == "" || len(m.SHA256) != 64 {
			t.Fatalf("a default model isn't pinned: %+v", m)
		}
	}
	if c.whisperModel() != "/models/whisper/ggml-base-q5_1.bin" || c.Models[2].Path != "/models/piper/en_US-lessac-medium.onnx.json" {
		t.Fatalf("paths: %+v", c.Models)
	}
	// Another whisper model without a checksum is used unchecked, never
	// with the default's checksum; another voice brings its own.
	env := map[string]string{"VOICE_WHISPER_MODEL_URL": "https://example.com/ggml-small.bin?download=1", "VOICE_PIPER_VOICE": "de_DE-thorsten-medium",
		"VOICE_PIPER_VOICE_URL": "https://example.com/de.onnx", "VOICE_PIPER_VOICE_SHA256": "abc"}
	c = configFromEnv(func(k string) string { return env[k] })
	if c.Models[0].SHA256 != "" || c.whisperModel() != "/models/whisper/ggml-small.bin" ||
		c.Models[1].URL != "https://example.com/de.onnx" || c.Models[1].SHA256 != "abc" || c.Models[2].URL != "" ||
		c.Models[1].Path != "/models/piper/de_DE-thorsten-medium.onnx" {
		t.Fatalf("custom: %+v", c.Models)
	}
}
