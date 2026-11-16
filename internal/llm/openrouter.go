package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// OpenRouter is a Client for https://openrouter.ai (or any OpenAI-compatible
// endpoint). The key is looked up on every call so a key added in chat
// takes effect immediately.
type OpenRouter struct {
	BaseURL string
	Key     func(ctx context.Context) (string, error)
	HTTP    *http.Client
}

func NewOpenRouter(baseURL string, key func(ctx context.Context) (string, error)) *OpenRouter {
	return &OpenRouter{BaseURL: strings.TrimRight(baseURL, "/"), Key: key, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

type orRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	Tools          []ToolDef      `json:"tools,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
	Usage          map[string]any `json:"usage"`
}

type orResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenRouter) Chat(ctx context.Context, req Request) (*Response, error) {
	key, err := o.Key(ctx)
	if err != nil || key == "" {
		return nil, ErrNoKey
	}
	body := orRequest{
		Model: req.Model, Messages: req.Messages, Tools: req.Tools,
		Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		Usage: map[string]any{"include": true},
	}
	if req.JSON {
		body.ResponseFormat = map[string]any{"type": "json_object"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+key)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Title", "Thicket")
	resp, err := o.HTTP.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	var out orResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unreadable model response: %w", err)
	}
	if out.Error != nil {
		// OpenRouter sometimes reports upstream failures inside a 200.
		return nil, &APIError{Status: 502, Message: out.Error.Message}
	}
	if len(out.Choices) == 0 {
		return nil, &APIError{Status: 502, Message: "model returned no choices"}
	}
	return &Response{Message: out.Choices[0].Message, Model: out.Model, Usage: out.Usage}, nil
}

func errorMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// CheckKey validates an OpenRouter key against the API. Success is checked
// in code, not assumed.
func (o *OpenRouter) CheckKey(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/key", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	return nil
}

// Router picks the model, retries transient failures, and falls back to a
// secondary model when the primary keeps failing.
type Router struct {
	Client   Client
	Primary  func(ctx context.Context) string
	Fallback func(ctx context.Context) string
	Log      *slog.Logger
	// OnFallback is called when the fallback model was used.
	OnFallback func(ctx context.Context, from, to string, cause error)
	Retries    int
	Backoff    time.Duration
}

func (r *Router) Chat(ctx context.Context, req Request) (*Response, error) {
	primary := req.Model
	if primary == "" {
		primary = r.Primary(ctx)
	}
	resp, err := r.try(ctx, req, primary)
	if err == nil || req.NoFallback || Fatal(err) || ctx.Err() != nil {
		return resp, err
	}
	fb := ""
	if r.Fallback != nil {
		fb = r.Fallback(ctx)
	}
	if fb == "" || fb == primary {
		return nil, err
	}
	r.Log.Warn("primary model failed, falling back", "from", primary, "to", fb, "err", err)
	resp, err2 := r.try(ctx, req, fb)
	if err2 != nil {
		return nil, fmt.Errorf("primary %s failed (%v); fallback %s failed too: %w", primary, err, fb, err2)
	}
	if r.OnFallback != nil {
		r.OnFallback(ctx, primary, fb, err)
	}
	return resp, nil
}

func (r *Router) try(ctx context.Context, req Request, model string) (*Response, error) {
	req.Model = model
	backoff := r.Backoff
	if backoff == 0 {
		backoff = time.Second
	}
	var err error
	for attempt := 0; attempt <= r.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		var resp *Response
		resp, err = r.Client.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		if !Retryable(err) {
			// A 400 from this model may be model-specific (e.g. no tool
			// support), so let the caller try the fallback.
			return nil, err
		}
	}
	return nil, err
}

// IsBudget reports whether err is a budget refusal.
func IsBudget(err error) bool {
	var be *BudgetError
	return errors.As(err, &be)
}

// BudgetError is returned when a spend cap would be exceeded.
type BudgetError struct{ Reason string }

func (e *BudgetError) Error() string { return "spend cap reached: " + e.Reason }
