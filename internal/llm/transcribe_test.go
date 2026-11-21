package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranscriptionClient(t *testing.T) {
	var auth, model, lang, name, format string
	var audio []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("form: %v", err)
		}
		model, lang, format = r.FormValue("model"), r.FormValue("language"), r.FormValue("response_format")
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Errorf("file: %v", err)
			return
		}
		name = h.Filename
		audio, _ = io.ReadAll(f)
		switch string(audio) {
		case "loading":
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"message":"the voice plugin is still fetching its models"}}`))
		case "plain":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("  hello in plain text\n"))
		case "silence":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"text":"  "}`))
		case "odd":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"result":"?"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"text":" Remind me to call Maya at five. "}`))
		}
	}))
	defer srv.Close()
	c := &TranscriptionClient{Key: func(_ context.Context, base string) (string, error) { return "tok-for-" + base, nil }}
	ctx := context.Background()

	tr, err := c.Transcribe(ctx, TranscriptionRequest{URL: srv.URL + "/v1/", Audio: []byte("OggS"), Filename: "voice.ogg", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "Remind me to call Maya at five." || tr.Model != "whisper-1" || tr.Hosted {
		t.Fatalf("transcript: %+v", tr)
	}
	if auth != "Bearer tok-for-"+srv.URL+"/v1" || model != "whisper-1" || lang != "en" || name != "voice.ogg" || format != "json" || string(audio) != "OggS" {
		t.Fatalf("request: auth=%q model=%q lang=%q name=%q format=%q audio=%q", auth, model, lang, name, format, audio)
	}
	if tr, err := c.Transcribe(ctx, TranscriptionRequest{URL: srv.URL + "/v1", Model: "large-v3", Audio: []byte("plain")}); err != nil || tr.Text != "hello in plain text" || model != "large-v3" {
		t.Fatalf("plain: %+v %v (model %q)", tr, err, model)
	}
	var ae *APIError
	if _, err := c.Transcribe(ctx, TranscriptionRequest{URL: srv.URL + "/v1", Audio: []byte("loading")}); !errors.As(err, &ae) || ae.Status != 503 || !strings.Contains(ae.Message, "fetching its models") {
		t.Fatalf("loading: %v", err)
	}
	if _, err := c.Transcribe(ctx, TranscriptionRequest{URL: srv.URL + "/v1", Audio: []byte("silence")}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("silence: %v", err)
	}
	if _, err := c.Transcribe(ctx, TranscriptionRequest{URL: srv.URL + "/v1", Audio: []byte("odd")}); err == nil {
		t.Fatal("a body without text")
	}
	if _, err := c.Transcribe(ctx, TranscriptionRequest{Audio: []byte("x")}); err == nil {
		t.Fatal("no address")
	}
	// OpenRouter needs a key.
	none := &TranscriptionClient{Key: func(context.Context, string) (string, error) { return "", nil }}
	if _, err := none.Transcribe(ctx, TranscriptionRequest{URL: DefaultURL, Audio: []byte("x")}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}
