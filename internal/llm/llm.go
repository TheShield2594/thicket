// Package llm talks to chat models through OpenRouter's OpenAI-compatible
// API, with retries and fallback to a secondary model.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Annotations are OpenRouter's parsed copies of PDFs, returned on an
	// assistant message. Sending them back on that message in the next
	// call of the same turn skips parsing the PDF again.
	Annotations json.RawMessage `json:"annotations,omitempty"`
	// Images and Files go with a user message. When set, the message is
	// sent as content parts: Content first, then images, then files.
	// They are never stored, only sent with the turn they arrived in.
	Images []Image `json:"-"`
	Files  []File  `json:"-"`
}

// Image is a picture for a vision model, as an https or data: URL.
type Image struct {
	URL string
}

// File is a document (a PDF) as a data: URL. OpenRouter reads it natively
// or parses it, depending on the model and Request.PDFEngine.
type File struct {
	Name string
	URL  string
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	File     *filePart `json:"file,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

// MarshalJSON sends plain string content unless the message has media.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 && len(m.Files) == 0 {
		return json.Marshal(plain(m))
	}
	parts := []contentPart{}
	if m.Content != "" {
		parts = append(parts, contentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: img.URL}})
	}
	for _, f := range m.Files {
		parts = append(parts, contentPart{Type: "file", File: &filePart{Filename: f.Name, FileData: f.URL}})
	}
	return json.Marshal(struct {
		plain
		Content []contentPart `json:"content"`
	}{plain(m), parts})
}

// HasImages reports whether any message carries images.
func HasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// HasFiles reports whether any message carries files.
func HasFiles(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Files) > 0 {
			return true
		}
	}
	return false
}

// HasMedia reports whether any message carries images or files.
func HasMedia(msgs []Message) bool { return HasImages(msgs) || HasFiles(msgs) }

// ContextExceeded reports whether err says the prompt didn't fit the
// model's context window.
func ContextExceeded(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 413 && ae.Status != 422 && ae.Status != 500) {
		return false
	}
	m := strings.ToLower(ae.Message)
	for _, s := range []string{"context length", "context window", "maximum context", "context_length", "too many tokens", "prompt is too long", "exceeds the context", "input is too long"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// ToolsRejected reports whether err looks like the model refusing tool
// definitions (common with small local models).
func ToolsRejected(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 422 && ae.Status != 500) {
		return false
	}
	m := strings.ToLower(ae.Message)
	return strings.Contains(m, "tool") || strings.Contains(m, "function call")
}

// MediaRejected reports whether err looks like the model refusing image
// or file input (OpenRouter answers 404 "no endpoints that support image
// input", providers 400/415/422), as opposed to an outage or a bad key.
func MediaRejected(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ToolsRejected(err) || ContextExceeded(err) {
		return false // other recoveries handle these, keeping the media
	}
	switch ae.Status {
	case 400, 404, 413, 415, 422:
		return true
	}
	return false
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	// Model may be empty: the router fills in the primary model.
	Model       string
	Messages    []Message
	Tools       []ToolDef
	Temperature *float64
	MaxTokens   int
	JSON        bool // ask for a JSON object response
	// NoFallback disables falling back to the secondary model.
	NoFallback bool
	// PDFEngine picks how OpenRouter reads PDFs: "native", "cloudflare-ai"
	// or "mistral-ocr". Empty lets OpenRouter choose (native if the model
	// reads files, else OCR). Only sent when a message carries a file.
	PDFEngine string
	// WebResults > 0 has OpenRouter search the web first and give the
	// model that many results (its "web" plugin). OpenRouter only.
	WebResults int
}

type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
}

type Response struct {
	Message Message
	Model   string
	Usage   Usage
}

type Client interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

// APIError is a non-2xx response from the model provider.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("model API error %d: %s", e.Status, e.Message) }

// Retryable reports whether trying again (or another model) might help.
func Retryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 408 || ae.Status == 429 || ae.Status >= 500
	}
	return !errors.Is(err, ErrNoKey) && !errors.Is(err, context.Canceled)
}

// Fatal reports errors no other model will fix (bad key, no credits).
func Fatal(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 401 || ae.Status == 402 || ae.Status == 403
	}
	return errors.Is(err, ErrNoKey)
}

var ErrNoKey = errors.New("no model API key configured")

// Meta travels with a request through the context so usage and budgets can
// be attributed to a user, character and job.
type Meta struct {
	UserID    int64
	Character string
	JobID     int64
	Purpose   string // chat | job | memory | consistency | preview | proactive | document | voice-check
	// Origin is "heartbeat" for work Thicket starts by itself (jobs,
	// check-ins, follow-ups, memory review) or "task" for work a person
	// asked for. Empty means it follows from the purpose the call
	// started with (see OriginOf).
	Origin string
}

const (
	OriginHeartbeat = "heartbeat"
	OriginTask      = "task"
)

// OriginOf says whether a call is heartbeat or task work.
func OriginOf(m Meta) string {
	if m.Origin != "" {
		return m.Origin
	}
	switch m.Purpose {
	case "job", "proactive", "memory":
		return OriginHeartbeat
	}
	return OriginTask
}

type metaKey struct{}

func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

func MetaFrom(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey{}).(Meta)
	return m
}

// WithPurpose returns ctx with the purpose field of its Meta replaced.
// The origin stays what the outer call's purpose made it: a consistency
// check inside a scheduled job is still heartbeat work.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	m := MetaFrom(ctx)
	m.Origin = OriginOf(m)
	m.Purpose = purpose
	return WithMeta(ctx, m)
}
