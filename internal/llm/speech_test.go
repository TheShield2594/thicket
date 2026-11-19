package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSpeechClient(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		if got["voice"] == "nobody" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"unknown voice"}}`))
			return
		}
		if got["voice"] == "quiet" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"error":{"message":"model is loading"}}`))
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		w.Write([]byte("OggS spoken"))
	}))
	defer srv.Close()
	c := &SpeechClient{URL: func(context.Context) string { return srv.URL + "/v1/" },
		Key: func(context.Context, string) (string, error) { return "local-key", nil }}
	ctx := context.Background()

	sp, err := c.Speak(ctx, SpeechRequest{Model: "kokoro", Voice: "af_heart", Input: "Hello there.", Format: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if string(sp.Audio) != "OggS spoken" || sp.ContentType != "audio/ogg" || sp.Hosted || sp.Model != "kokoro" {
		t.Fatalf("speech: %+v", sp)
	}
	if got["model"] != "kokoro" || got["voice"] != "af_heart" || got["input"] != "Hello there." || got["response_format"] != "opus" || auth != "Bearer local-key" {
		t.Fatalf("request: %v (auth %q)", got, auth)
	}

	_, err = c.Speak(ctx, SpeechRequest{Voice: "nobody", Input: "x"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "unknown voice" {
		t.Fatalf("error: %v", err)
	}
	if _, err := c.Speak(ctx, SpeechRequest{Voice: "quiet", Input: "x"}); err == nil {
		t.Fatal("a JSON body is not audio")
	}

	// OpenRouter needs a key.
	or := &SpeechClient{URL: func(context.Context) string { return DefaultURL },
		Key: func(context.Context, string) (string, error) { return "", nil }}
	if _, err := or.Speak(ctx, SpeechRequest{Input: "x"}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	// OpenRouter takes mp3 or pcm only: a channel's opus becomes mp3.
	or = &SpeechClient{URL: func(context.Context) string { return DefaultURL },
		Key: func(context.Context, string) (string, error) { return "sk-or", nil },
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.URL.Scheme, r.URL.Host, r.URL.Path = "http", strings.TrimPrefix(srv.URL, "http://"), "/v1/audio/speech"
			return http.DefaultTransport.RoundTrip(r)
		})}}
	if sp, err := or.Speak(ctx, SpeechRequest{Voice: "alloy", Input: "Hi.", Format: "opus"}); err != nil || !sp.Hosted || got["response_format"] != "mp3" {
		t.Fatalf("hosted speech: %+v %v (asked %v)", sp, err, got["response_format"])
	}
	none := &SpeechClient{URL: func(context.Context) string { return "" }}
	if _, err := none.Speak(ctx, SpeechRequest{Input: "x"}); err == nil {
		t.Fatal("no server set")
	}
}
