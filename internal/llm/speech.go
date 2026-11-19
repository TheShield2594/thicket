package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SpeechRequest asks for text read aloud. It is the shape of the OpenAI
// speech endpoint (POST {base}/audio/speech), which OpenRouter and local
// speech servers (Kokoro-FastAPI, openedai-speech, LocalAI, Speaches)
// accept too.
type SpeechRequest struct {
	Model string
	Voice string
	Input string
	// Format is the response_format: "opus" (Ogg Opus, what Telegram
	// voice notes are), "mp3", "wav" and so on.
	Format string
}

// Speech is the audio a speech server made.
type Speech struct {
	Audio       []byte
	ContentType string // as the server reported it; may be empty
	Model       string
	// Hosted is true when the audio came from OpenRouter (it costs money;
	// a local server doesn't).
	Hosted bool
}

// Speaker turns text into speech.
type Speaker interface {
	Speak(ctx context.Context, req SpeechRequest) (*Speech, error)
}

// maxSpeechBytes bounds what a speech server may send back: minutes of
// compressed audio, far more than a reply needs.
const maxSpeechBytes = 20 << 20

// SpeechClient is a Speaker over an OpenAI-compatible speech endpoint.
// Like OpenRouter, it looks up the address and key on every call.
type SpeechClient struct {
	// URL returns the base URL (".../v1") for each call.
	URL  func(ctx context.Context) string
	Key  func(ctx context.Context, base string) (string, error)
	HTTP *http.Client
}

func (s *SpeechClient) Speak(ctx context.Context, req SpeechRequest) (*Speech, error) {
	base := strings.TrimRight(s.URL(ctx), "/")
	if base == "" {
		return nil, fmt.Errorf("no speech server is set (the speech_url setting)")
	}
	hosted := IsOpenRouter(base)
	key := ""
	if s.Key != nil {
		k, err := s.Key(ctx, base)
		if hosted && (err != nil || k == "") {
			return nil, ErrNoKey
		}
		key = k
	}
	body, err := json.Marshal(map[string]any{"model": req.Model, "input": req.Input, "voice": req.Voice, "response_format": req.Format})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Title", "Thicket")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	ct := resp.Header.Get("Content-Type")
	if len(raw) > maxSpeechBytes {
		return nil, fmt.Errorf("the speech server sent too much audio")
	}
	if len(raw) == 0 || strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/") {
		// Some servers answer errors with 200 and a JSON body.
		return nil, &APIError{Status: 502, Message: "the speech server sent no audio: " + errorMessage(raw)}
	}
	return &Speech{Audio: raw, ContentType: ct, Model: req.Model, Hosted: hosted}, nil
}
