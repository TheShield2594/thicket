package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// maxUploadBytes is OpenAI's limit for one recording.
	maxUploadBytes = 25 << 20
	// maxInputChars is OpenAI's limit for text to speak.
	maxInputChars = 4096
)

type server struct {
	cfg   config
	tools *tools
	http  *http.Client
	jobs  chan struct{} // one slot per transcription or synthesis running

	mu     sync.Mutex
	status string // starting | ready | error
	detail string
}

func newServer(c config) *server {
	return &server{cfg: c, tools: newTools(c), http: &http.Client{}, jobs: make(chan struct{}, max(c.Jobs, 1)), status: "starting"}
}

func (s *server) setStatus(status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.detail = status, detail
}

func (s *server) state() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.detail
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	// /health answers without a token: Thicket uses it to see whether the
	// plugin is running at all.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.auth(s.transcriptions))
	mux.HandleFunc("POST /v1/audio/speech", s.auth(s.speech))
	mux.HandleFunc("POST /mcp", s.auth(s.mcp))
	return mux
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	status, detail := s.state()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": detail, "version": version, "voices": s.tools.voices()})
}

// authorized checks the bearer token against the shared file, read on
// every request so a new token takes effect without a restart. With no
// token file nothing is allowed.
func (s *server) authorized(r *http.Request) bool {
	b, err := os.ReadFile(s.cfg.TokenFile)
	want := strings.TrimSpace(string(b))
	if err != nil || len(want) < 32 {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// apiError answers the way OpenAI's API does, so clients show the message.
func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(status)}})
}

// ready refuses work until the models are in place.
func (s *server) ready(w http.ResponseWriter) bool {
	status, detail := s.state()
	if status == "ready" {
		return true
	}
	msg := "the voice plugin is still fetching its models"
	if detail != "" {
		msg += " (" + detail + ")"
	}
	w.Header().Set("Retry-After", "30")
	apiError(w, http.StatusServiceUnavailable, msg)
	return false
}

// slot waits for a free job slot; release it when done.
func (s *server) slot(ctx context.Context) (release func(), err error) {
	select {
	case s.jobs <- struct{}{}:
		return func() { <-s.jobs }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) transcriptions(w http.ResponseWriter, r *http.Request) {
	if !s.ready(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "expected a multipart form with the recording in \"file\" (at most 25 MB)")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "no recording in \"file\"")
		return
	}
	audio, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	f.Close()
	if err != nil || len(audio) == 0 || len(audio) > maxUploadBytes {
		apiError(w, http.StatusBadRequest, "the recording is empty or larger than 25 MB")
		return
	}
	format := r.FormValue("response_format")
	if format != "" && format != "json" && format != "text" && format != "verbose_json" {
		apiError(w, http.StatusBadRequest, "response_format must be json, verbose_json or text")
		return
	}
	lang := strings.ToLower(strings.TrimSpace(r.FormValue("language")))
	if lang != "" && !validLanguage(lang) {
		apiError(w, http.StatusBadRequest, "language must be a code such as en")
		return
	}
	release, err := s.slot(r.Context())
	if err != nil {
		return
	}
	defer release()
	start := time.Now()
	text, err := s.tools.transcribe(r.Context(), audio, lang)
	if err != nil {
		log.Printf("transcription failed: %v", err)
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("transcribed %d bytes in %s", len(audio), time.Since(start).Round(time.Millisecond))
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"text": text})
}

func validLanguage(s string) bool {
	if s == "auto" {
		return true
	}
	if len(s) < 2 || len(s) > 3 {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func (s *server) speech(w http.ResponseWriter, r *http.Request) {
	if !s.ready(w) {
		return
	}
	var req struct {
		Model  string  `json:"model"`
		Input  string  `json:"input"`
		Voice  string  `json:"voice"`
		Format string  `json:"response_format"`
		Speed  float64 `json:"speed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "expected JSON with input, voice and response_format")
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		apiError(w, http.StatusBadRequest, "input is empty")
		return
	}
	if utf8.RuneCountInString(req.Input) > maxInputChars {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("input is longer than %d characters", maxInputChars))
		return
	}
	if req.Speed != 0 && (req.Speed < 0.25 || req.Speed > 4) {
		apiError(w, http.StatusBadRequest, "speed must be between 0.25 and 4")
		return
	}
	if req.Format == "" {
		req.Format = "mp3"
	}
	if _, ok := formats[req.Format]; !ok {
		apiError(w, http.StatusBadRequest, "response_format must be one of mp3, opus, aac, flac, wav, pcm")
		return
	}
	release, err := s.slot(r.Context())
	if err != nil {
		return
	}
	defer release()
	start := time.Now()
	audio, ct, err := s.tools.speak(r.Context(), req.Input, req.Voice, req.Speed, req.Format)
	if err != nil {
		log.Printf("speech failed: %v", err)
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("spoke %d characters as %s in %s", utf8.RuneCountInString(req.Input), req.Format, time.Since(start).Round(time.Millisecond))
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(audio)
}

// --- MCP: Thicket connects every plugin as an integration. This one has
// a single read-only tool; Thicket itself uses the audio endpoints. ---

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *server) mcp(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, err := s.answer(req)
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if err != nil {
		resp["error"] = map[string]any{"code": -32601, "message": err.Error()}
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) answer(req rpcRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "thicket-voice", "version": version},
		}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "list_voices",
			"description": "Lists the local voices a sprig can speak in (a character file's voice.sounds_like) and which one is the default.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"annotations": map[string]any{"readOnlyHint": true},
		}}}, nil
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "list_voices" {
			return nil, errors.New("unknown tool")
		}
		voices := s.tools.voices()
		text := "No voices yet: the voice plugin is still fetching its models."
		if len(voices) > 0 {
			text = fmt.Sprintf("Local voices: %s. Default: %s. Any other name gets the default.", strings.Join(voices, ", "), s.cfg.Voice)
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	case "ping":
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("method %s not found", req.Method)
}
