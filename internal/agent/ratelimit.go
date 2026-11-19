package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// Rate limits, per sprig and per integration, checked in code before a
// tool runs:
//
//   - limits.tool_calls_per_minute: tool calls a sprig makes (default 30)
//   - limits.actions_per_hour: actions a sprig takes without asking, that
//     is tools that change something, run at act (default 20)
//   - limits.integration_calls_per_minute: calls to one integration, from
//     every sprig together (default 60)
//
// Each can be set per sprig (limits.sprig.<id>.tool_calls_per_minute,
// limits.sprig.<id>.actions_per_hour) or per integration
// (limits.integration.<name>.calls_per_minute); 0 means no limit. A call
// over a limit isn't run: the model is told, the activity log says so,
// and the sprig's owner gets one inbox item per limit (repeats bump it).
// The counts are kept in memory, so a restart starts them afresh.

type rateLimits struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// count returns how many hits key had in the last per.
func (r *rateLimits) count(key string, per time.Duration, now time.Time) int {
	if r.hits == nil {
		r.hits = map[string][]time.Time{}
	}
	h := r.hits[key]
	i := 0
	for i < len(h) && now.Sub(h[i]) >= per {
		i++
	}
	h = h[i:]
	if len(h) == 0 {
		delete(r.hits, key)
	} else {
		r.hits[key] = h
	}
	return len(h)
}

type limitCheck struct {
	key, setting, what string
	limit              int
	per                time.Duration
}

// limit reads a limit, a per-target override first.
func (e *Engine) limit(ctx context.Context, override, general string) int {
	if v := e.Settings.String(ctx, override); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return e.Settings.Int(ctx, general)
}

// rateLimited checks the limits for a call about to run and records it.
// It returns what the model is told when a limit is reached, else "".
// act is whether this call acts without having been approved.
func (e *Engine) rateLimited(ctx context.Context, t *turn, tool *Tool, act bool) string {
	act = act && changes(tool)
	checks := []limitCheck{{
		key: "tools:" + t.char.ID, setting: "limits.tool_calls_per_minute", what: "tool calls a minute",
		limit: e.limit(ctx, "limits.sprig."+t.char.ID+".tool_calls_per_minute", "limits.tool_calls_per_minute"), per: time.Minute,
	}}
	if act {
		checks = append(checks, limitCheck{
			key: "acts:" + t.char.ID, setting: "limits.actions_per_hour", what: "actions an hour without asking",
			limit: e.limit(ctx, "limits.sprig."+t.char.ID+".actions_per_hour", "limits.actions_per_hour"), per: time.Hour,
		})
	}
	if srv, ok := strings.CutPrefix(tool.Scope, "mcp:"); ok {
		checks = append(checks, limitCheck{
			key: "mcp:" + srv, setting: "limits.integration_calls_per_minute", what: "calls a minute to the " + srv + " integration",
			limit: e.limit(ctx, "limits.integration."+srv+".calls_per_minute", "limits.integration_calls_per_minute"), per: time.Minute,
		})
	}
	now := e.now()
	e.limits.mu.Lock()
	for _, c := range checks {
		if c.limit > 0 && e.limits.count(c.key, c.per, now) >= c.limit {
			e.limits.mu.Unlock()
			e.limitHit(ctx, t, tool, c)
			return fmt.Sprintf("Error: rate limit reached (%d %s), so %s was NOT run. Stop calling tools and tell the user; the owner has been told.", c.limit, c.what, tool.Label())
		}
	}
	for _, c := range checks {
		if c.limit > 0 {
			e.limits.hits[c.key] = append(e.limits.hits[c.key], now)
		}
	}
	e.limits.mu.Unlock()
	return ""
}

func (e *Engine) limitHit(ctx context.Context, t *turn, tool *Tool, c limitCheck) {
	e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, fmt.Sprintf("rate limit: %s refused (%d %s)", tool.Name, c.limit, c.what), "")
	e.deadLetter(ctx, store.InboxItem{UserID: heldOwner(t), Character: t.char.ID, Kind: store.InboxFailed,
		Ref:   "limit:" + t.char.ID + ":" + c.key,
		Title: fmt.Sprintf("%s hit a rate limit: %d %s", t.char.DisplayName(), c.limit, c.what),
		Detail: fmt.Sprintf("%s wasn't run. If this is expected, raise the limit (setting %s, or a per-sprig or per-integration override); "+
			"if not, look at what %s was doing, or pause it.", tool.Label(), c.setting, t.char.DisplayName())})
}
