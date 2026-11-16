package app

import (
	"context"
	"encoding/json"

	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
)

// redactingModel is the last line of defence at the model boundary: no
// known secret value (or credential-shaped string) goes out in a prompt,
// and none comes back in a reply. Secrets are kept out of prompts long
// before this; it catches whatever a new code path forgets.
type redactingModel struct {
	next llm.Client
	red  *secrets.Redactor
}

func (r redactingModel) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	msgs := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = r.message(m)
	}
	req.Messages = msgs
	resp, err := r.next.Chat(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Reply text is cleaned; tool call arguments are left for the tools'
	// own checks, which refuse secrets outright (a skill with a key in it
	// is refused, not saved with "[redacted]" in its steps).
	out := *resp
	out.Message.Content = r.red.Redact(resp.Message.Content)
	return &out, nil
}

func (r redactingModel) message(m llm.Message) llm.Message {
	m.Content = r.red.Redact(m.Content)
	if len(m.ToolCalls) > 0 {
		calls := make([]llm.ToolCall, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			c.Function.Arguments = r.red.Redact(c.Function.Arguments)
			calls[i] = c
		}
		m.ToolCalls = calls
	}
	if len(m.Annotations) > 0 {
		// Parsed PDF text rides along here. The replacement has no quotes
		// or backslashes, so the JSON stays valid.
		if s := r.red.Redact(string(m.Annotations)); s != string(m.Annotations) && json.Valid([]byte(s)) {
			m.Annotations = json.RawMessage(s)
		}
	}
	return m
}
