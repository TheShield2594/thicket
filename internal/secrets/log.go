package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// LogHandler wraps a slog.Handler and redacts secrets from messages and
// string attributes, so a stray key in an error message never hits the log.
type LogHandler struct {
	next slog.Handler
	red  *Redactor
}

func NewLogHandler(next slog.Handler, red *Redactor) *LogHandler {
	return &LogHandler{next: next, red: red}
}

func (h *LogHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.red.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, nr)
}

func (h *LogHandler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.red.Redact(v.String()))
	case slog.KindAny:
		// Anything else (errors, structs, maps, raw JSON) is rendered as
		// text and redacted, so no value type can smuggle a secret through.
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, h.red.Redact(x.Error()))
		case []byte:
			return slog.String(a.Key, h.red.Redact(string(x)))
		case json.RawMessage:
			return slog.String(a.Key, h.red.Redact(string(x)))
		case fmt.Stringer:
			return slog.String(a.Key, h.red.Redact(x.String()))
		default:
			return slog.String(a.Key, h.red.Redact(fmt.Sprintf("%+v", x)))
		}
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, 0, len(g))
		for _, x := range g {
			out = append(out, h.attr(x))
		}
		return slog.Group(a.Key, out...)
	}
	return a
}

func (h *LogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = h.attr(a)
	}
	return &LogHandler{next: h.next.WithAttrs(out), red: h.red}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{next: h.next.WithGroup(name), red: h.red}
}
