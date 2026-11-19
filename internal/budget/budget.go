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
	Next llm.Client
	// Voice makes spoken replies; Speak meters it like Next.
	Voice    llm.Speaker
	DB       *store.Store
	Settings *settings.Settings
	Log      *slog.Logger
	OnAlert  func(ctx context.Context, a Alert)
	Now      func() time.Time
	// Paused, if set, refuses every model call while Thicket or the
	// calling sprig is paused (the kill switch): a backstop under every
	// other check, so nothing that reaches a model runs.
	Paused func(ctx context.Context, character string) error

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

// HeartbeatPause returns why background work (scheduled jobs, check-ins,
// follow-ups) for this sprig should be held back, or "" to go ahead. At a
// cap, heartbeats aren't started at all, so they don't fail and pile up
// error reports; the 100% alert already told the admins. character may be
// empty.
func (g *Guard) HeartbeatPause(ctx context.Context, character string) string {
	if g == nil {
		return ""
	}
	cs, err := g.checks(ctx, llm.Meta{Character: character})
	if err != nil {
		return "" // the call itself will be checked again, failing closed
	}
	for _, c := range cs {
		if c.limit > 0 && c.spent >= c.limit {
			g.capAlert(ctx, c, c.spent) // admins hear even if no chat call ever hits the cap
			return fmt.Sprintf("the %s spend cap ($%.2f) is reached", c.name, c.limit)
		}
	}
	return ""
}

// capAlert is the 100% alert; its key makes it once per cap and period.
func (g *Guard) capAlert(ctx context.Context, c capCheck, spent float64) {
	g.alert(ctx, Alert{
		Key:     "budget:" + c.name + ":" + c.period + ":100",
		Level:   "error",
		Message: fmt.Sprintf("We've hit the %s spend cap ($%.2f of $%.2f). I'm pausing model calls until it resets, or you can raise it (tell me the new cap).", c.name, spent, c.limit),
	})
}

// admit refuses a call while paused, past a cap, or while usage can't be
// recorded. It returns the caps it checked, for record.
func (g *Guard) admit(ctx context.Context, m llm.Meta) ([]capCheck, error) {
	if g.Paused != nil {
		if err := g.Paused(ctx, m.Character); err != nil {
			return nil, err
		}
	}
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
			g.capAlert(ctx, c, c.spent)
			return nil, &llm.BudgetError{Reason: fmt.Sprintf("%s cap $%.2f reached", c.name, c.limit)}
		}
	}
	return cs, nil
}

// Speak makes a spoken reply, under the same caps as model calls. The
// speech endpoint reports no cost, so speech from OpenRouter is priced by
// the speech_usd_per_1k_chars setting; a local speech server is free.
func (g *Guard) Speak(ctx context.Context, req llm.SpeechRequest) (*llm.Speech, error) {
	if g.Voice == nil {
		return nil, fmt.Errorf("spoken replies aren't set up")
	}
	m := llm.MetaFrom(ctx)
	cs, err := g.admit(ctx, m)
	if err != nil {
		return nil, err
	}
	sp, err := g.Voice.Speak(ctx, req)
	if err != nil {
		return nil, err
	}
	cost := 0.0
	if sp.Hosted {
		cost = g.Settings.Float(ctx, "speech_usd_per_1k_chars") * float64(len([]rune(req.Input))) / 1000
	}
	g.record(ctx, cs, store.Usage{UserID: m.UserID, Character: m.Character, JobID: m.JobID, Purpose: m.Purpose,
		Origin: llm.OriginOf(m), Model: sp.Model, CostUSD: cost})
	return sp, nil
}

func (g *Guard) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	m := llm.MetaFrom(ctx)
	cs, err := g.admit(ctx, m)
	if err != nil {
		return nil, err
	}
	resp, err := g.Next.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	u := store.Usage{
		UserID: m.UserID, Character: m.Character, JobID: m.JobID, Purpose: m.Purpose, Origin: llm.OriginOf(m),
		Model: resp.Model, PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, CostUSD: resp.Usage.Cost,
	}
	if req.Mix != nil {
		u.PromptChars = max(llm.PromptChars(req.Messages), 1)
		u.HistoryChars = min(req.Mix.HistoryChars, u.PromptChars)
		u.DocumentChars = min(req.Mix.DocumentChars, u.PromptChars-u.HistoryChars)
	}
	g.record(ctx, cs, u)
	return resp, nil
}

// record stores a paid call's usage and raises the 80% and 100% alerts it
// crossed.
func (g *Guard) record(ctx context.Context, cs []capCheck, u store.Usage) {
	// The call is paid for even if the caller has gone away: record it
	// regardless of cancellation, and keep it for retry if the write fails.
	if rerr := g.DB.RecordUsage(context.WithoutCancel(ctx), u); rerr != nil {
		g.Log.Error("record usage; will retry before the next call", "err", rerr)
		g.mu.Lock()
		g.pending = append(g.pending, u)
		g.mu.Unlock()
	}
	for _, c := range cs {
		after := c.spent + u.CostUSD
		if c.limit > 0 && after >= c.limit && c.spent < c.limit {
			g.capAlert(ctx, c, after) // this call crossed the cap
			continue
		}
		if c.limit > 0 && after >= 0.8*c.limit && c.spent < 0.8*c.limit {
			g.alert(ctx, Alert{
				Key:     "budget:" + c.name + ":" + c.period + ":80",
				Level:   "warn",
				Message: fmt.Sprintf("Heads up: we've used %.0f%% of the %s budget ($%.2f of $%.2f).", 100*after/c.limit, c.name, after, c.limit) + g.savingTip(ctx),
			})
		}
	}
}

func (g *Guard) alert(ctx context.Context, a Alert) {
	if g.OnAlert != nil {
		g.OnAlert(ctx, a)
	}
}

// savingTip says how much of this month's chat spend went on re-sending
// the conversation and kept documents, and which settings cut it. Empty
// when that isn't known or isn't much.
func (g *Guard) savingTip(ctx context.Context) string {
	mix, err := g.DB.PromptMixSince(ctx, monthStart(g.now()))
	if err != nil || mix.Calls == 0 {
		return ""
	}
	tip := ""
	if h := mix.HistoryShare(); h >= 0.2 {
		cur := "automatic"
		if n := g.Settings.Int(ctx, "history_tokens"); n > 0 {
			cur = fmt.Sprintf("%d tokens", n)
		}
		tip += fmt.Sprintf(" About %.0f%% of chat spend this month went on re-sending earlier conversation with each reply; lowering history_tokens (now %s) makes every reply cheaper, at the cost of sprigs seeing less of it word for word.", 100*h, cur)
	}
	if d := mix.DocShare(); d >= 0.1 {
		tip += fmt.Sprintf(" About %.0f%% went on PDF text kept for follow-up questions; lowering document_chars (now %d) keeps less of each PDF.", 100*d, g.Settings.Int(ctx, "document_chars"))
	}
	return tip
}

// MixLine describes how chat spend split between earlier conversation
// and kept documents, or "" if no chat calls were measured.
func MixLine(m store.PromptMix) string {
	if m.Calls == 0 || m.CostUSD <= 0 {
		return ""
	}
	calls := fmt.Sprintf("%d model calls", m.Calls)
	if m.Calls == 1 {
		calls = "1 model call"
	}
	return fmt.Sprintf("Of $%.2f chat spend (%s measured), about %.0f%% (≈$%.2f) went on re-sending earlier conversation (history_tokens) and %.0f%% (≈$%.2f) on PDF text kept for follow-ups (document_chars).",
		m.CostUSD, calls, 100*m.HistoryShare(), m.HistoryUSD, 100*m.DocShare(), m.DocUSD)
}
