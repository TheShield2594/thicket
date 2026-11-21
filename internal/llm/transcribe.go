package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// TranscriptionRequest asks a speech-to-text server what a recording
// says. It is the shape of the OpenAI transcription endpoint
// (POST {base}/audio/transcriptions, multipart), which local servers
// (the thicket-voice plugin, whisper.cpp's server, Speaches, LocalAI)
// accept too.
type TranscriptionRequest struct {
	// URL is the server's base URL (".../v1").
	URL      string
	Model    string
	Audio    []byte
	Filename string // tells the server the format, e.g. voice.ogg
	Language string // optional, e.g. en
}

// Transcript is what a speech-to-text server heard.
type Transcript struct {
	Text  string
	Model string
	// Hosted is true when the transcript came from OpenRouter.
	Hosted bool
}

// Transcriber turns speech into text.
type Transcriber interface {
	Transcribe(ctx context.Context, req TranscriptionRequest) (*Transcript, error)
}

// TranscriptionClient is a Transcriber over an OpenAI-compatible
// transcription endpoint. It looks up the key for each call's address.
type TranscriptionClient struct {
	Key  func(ctx context.Context, base string) (string, error)
	HTTP *http.Client
}

func (c *TranscriptionClient) Transcribe(ctx context.Context, req TranscriptionRequest) (*Transcript, error) {
	base := strings.TrimRight(req.URL, "/")
	if base == "" {
		return nil, fmt.Errorf("no transcription server is set (the transcribe_url setting)")
	}
	hosted := IsOpenRouter(base)
	key := ""
	if c.Key != nil {
		k, err := c.Key(ctx, base)
		if hosted && (err != nil || k == "") {
			return nil, ErrNoKey
		}
		key = k
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	name := req.Filename
	if name == "" {
		name = "audio"
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(req.Audio); err != nil {
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = "whisper-1"
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("response_format", "json")
	if req.Language != "" {
		_ = mw.WriteField("language", req.Language)
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/transcriptions", &body)
	if err != nil {
		return nil, err
	}
	if key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	hreq.Header.Set("X-Title", "Thicket")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	text := ""
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		text = string(raw)
	} else {
		var out struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Text == nil {
			return nil, &APIError{Status: 502, Message: "the transcription server sent no text: " + errorMessage(raw)}
		}
		text = *out.Text
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("the transcription came back empty")
	}
	return &Transcript{Text: text, Model: model, Hosted: hosted}, nil
}
