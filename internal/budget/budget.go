// Package budget meters model usage and enforces spend caps.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// Alert is raised when spend crosses 80% or 100% of a cap.
type Alert struct {
	Key     string // dedupe key
	Level   string // warn | error
	Message string
}

// Guard wraps a model client: it refuses calls past a cap, records usage
// for every call, and raises alerts.
type Guard struct {
	Next     llm.Client
	DB       *store.Store
	Settings *settings.Settings
	Log      *slog.Logger
	OnAlert  func(ctx context.Context, a Alert)
	Now      func() time.Time

	mu      sync.Mutex
	pending []store.Usage // paid calls not yet written to the database
}

// flushPending retries usage rows that failed to record. Until they are
// written, spend totals are incomplete, so further calls are refused.
func (g *Guard) flushPending(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for len(g.pending) > 0 {
		if err := g.DB.RecordUsage(context.WithoutCancel(ctx), g.pending[0]); err != nil {
			return err
		}
		g.pending = g.pending[1:]
	}
	return nil
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

type capCheck struct {
	name   string
	limit  float64
	spent  float64
	period string
}

func (g *Guard) checks(ctx context.Context, m llm.Meta) ([]capCheck, error) {
	now := g.now()
	day, err := g.DB.SpendSince(ctx, dayStart(now), "")
	if err != nil {
		return nil, err
	}
	month, err := g.DB.SpendSince(ctx, monthStart(now), "")
	if err != nil {
		return nil, err
	}
	out := []capCheck{
		{"daily", g.Settings.Float(ctx, "budget.daily_usd"), day, dayStart(now).Format("2006-01-02")},
		{"monthly", g.Settings.Float(ctx, "budget.monthly_usd"), month, monthStart(now).Format("2006-01")},
	}
	if m.Character != "" {
		if lim := g.Settings.Float(ctx, "budget.character."+m.Character+".daily_usd"); lim > 0 {
			spent, err := g.DB.SpendSince(ctx, dayStart(now), m.Character)
			if err != nil {
				return nil, err
			}
			out = append(out, capCheck{m.Character + " daily", lim, spent, dayStart(now).Format("2006-01-02")})
		}
	}
	return out, nil
}

// Status summarises spend against caps, for chat and /health.
func (g *Guard) Status(ctx context.Context) (string, error) {
	cs, err := g.checks(ctx, llm.Meta{})
	if err != nil {
		return "", err
	}
	s := ""
	for _, c := range cs {
		lim := "no cap"
		if c.limit > 0 {
			lim = fmt.Sprintf("cap $%.2f", c.limit)
		}
		s += fmt.Sprintf("%s: $%.4f spent (%s)\n", c.name, c.spent, lim)
	}
	return s, nil
}

func (g *Guard) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	m := llm.MetaFrom(ctx)
	if err := g.flushPending(ctx); err != nil {
		g.alert(ctx, Alert{Key: "budget:accounting", Level: "error",
			Message: "I can't record what model calls cost right now (" + err.Error() + "), so I've paused model calls to keep the spend caps honest."})
		return nil, &llm.BudgetError{Reason: "usage accounting is failing"}
	}
	cs, err := g.checks(ctx, m)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.limit > 0 && c.spent >= c.limit {
			g.alert(ctx, Alert{
				Key:     "budget:" + c.name + ":" + c.period + ":100",
				Level:   "error",
				Message: fmt.Sprintf("We've hit the %s spend cap ($%.2f of $%.2f). I'm pausing model calls until it resets, or you can raise it (tell me the new cap).", c.name, c.spent, c.limit),
			})
			return nil, &llm.BudgetError{Reason: fmt.Sprintf("%s cap $%.2f reached", c.name, c.limit)}
		}
	}
	resp, err := g.Next.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	u := store.Usage{
		UserID: m.UserID, Character: m.Character, JobID: m.JobID, Purpose: m.Purpose,
		Model: resp.Model, PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, CostUSD: resp.Usage.Cost,
	}
	// The call is paid for even if the caller has gone away: record it
	// regardless of cancellation, and keep it for retry if the write fails.
	if rerr := g.DB.RecordUsage(context.WithoutCancel(ctx), u); rerr != nil {
		g.Log.Error("record usage; will retry before the next call", "err", rerr)
		g.mu.Lock()
		g.pending = append(g.pending, u)
		g.mu.Unlock()
	}
	for _, c := range cs {
		after := c.spent + resp.Usage.Cost
		if c.limit > 0 && after >= 0.8*c.limit && c.spent < 0.8*c.limit {
			g.alert(ctx, Alert{
				Key:     "budget:" + c.name + ":" + c.period + ":80",
				Level:   "warn",
				Message: fmt.Sprintf("Heads up: we've used %.0f%% of the %s budget ($%.2f of $%.2f).", 100*after/c.limit, c.name, after, c.limit),
			})
		}
	}
	return resp, nil
}

func (g *Guard) alert(ctx context.Context, a Alert) {
	if g.OnAlert != nil {
		g.OnAlert(ctx, a)
	}
}
