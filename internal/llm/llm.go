// Package llm talks to chat models through OpenRouter's OpenAI-compatible
// API, with retries and fallback to a secondary model.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
	Purpose   string // chat | job | memory | consistency | preview | proactive
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
func WithPurpose(ctx context.Context, purpose string) context.Context {
	m := MetaFrom(ctx)
	m.Purpose = purpose
	return WithMeta(ctx, m)
}
