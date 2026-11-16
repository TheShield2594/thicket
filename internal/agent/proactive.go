package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
)

// --- scheduled jobs (implements scheduler.Executor) ---

func (e *Engine) Location(ctx context.Context, userID int64) *time.Location {
	u, err := e.DB.UserByID(ctx, userID)
	if err != nil {
		return time.UTC
	}
	return u.Location()
}

func (e *Engine) RunJob(ctx context.Context, j store.Job, runID int64) scheduler.Result {
	start := e.now()
	user, err := e.DB.UserByID(ctx, j.UserID)
	if err != nil {
		return scheduler.Result{Err: fmt.Errorf("job owner not found: %w", err)}
	}
	char, ok := e.Chars.Get(j.Character)
	if !ok {
		return scheduler.Result{Err: fmt.Errorf("the sprig %q that owns this job no longer exists", j.Character)}
	}
	t := &turn{user: user, char: char, job: &j, query: j.Name + " " + j.Prompt}
	jctx := llm.WithMeta(ctx, llm.Meta{UserID: user.ID, Character: char.ID, JobID: j.ID, Purpose: "job"})
	jctx, cancel := context.WithTimeout(jctx, 5*time.Minute)
	defer cancel()
	out, err := e.runTurn(jctx, t, "")
	cost, _ := e.DB.JobSpendSince(ctx, j.ID, start.Add(-time.Second))
	if err != nil {
		return scheduler.Result{Err: err, Cost: cost}
	}
	if t.nothingToReport || out == "" {
		return scheduler.Result{Status: "empty", Cost: cost}
	}
	return scheduler.Result{Status: "ok", Output: out, Cost: cost}
}

// JobFinished delivers a job's result, or reports its failure loudly.
func (e *Engine) JobFinished(ctx context.Context, j store.Job, res scheduler.Result, next time.Time) {
	user, err := e.DB.UserByID(ctx, j.UserID)
	if err != nil {
		e.Log.Error("job finished for unknown user", "job", j.ID)
		return
	}
	char, ok := e.Chars.Get(j.Character)
	if !ok {
		char, _ = e.Chars.Host()
	}
	if char == nil {
		return
	}
	switch res.Status {
	case "ok":
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran", j.ID, j.Name), "")
		if err := e.sendToUser(ctx, user, char, res.Output); err != nil {
			e.Log.Error("deliver job output", "job", j.ID, "err", err)
			e.logActivity(ctx, "error", "job", char.ID, user.ID, fmt.Sprintf("job #%d ran but its message couldn't be delivered", j.ID), err.Error())
		}
	case "empty":
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran: nothing to report", j.ID, j.Name), "")
	default:
		errText := "unknown error"
		if res.Err != nil {
			errText = res.Err.Error()
		}
		e.logActivity(ctx, "error", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q failed", j.ID, j.Name), errText)
		// Same error for the same job is reported at most every 6 hours.
		key := fmt.Sprintf("jobfail:%d:%s", j.ID, truncate(errText, 80))
		if ok, _ := e.DB.ClaimNotice(ctx, key, 6*time.Hour); !ok {
			return
		}
		when := ""
		if !next.IsZero() {
			when = " I'll try again " + next.In(user.Location()).Format("Mon 2 Jan at 15:04") + "."
		}
		msg := fmt.Sprintf("⚠️ My job “%s” didn't work this time: %s.%s", j.Name, shortErr(res.Err), when)
		if res.Err == nil {
			msg = fmt.Sprintf("⚠️ My job “%s” didn't work this time.%s", j.Name, when)
		}
		if err := e.sendToUser(ctx, user, char, msg); err != nil {
			e.Log.Error("report job failure", "job", j.ID, "err", err)
		}
	}
}

// --- light initiative: follow-ups and check-ins ---

// RunInitiative checks for due follow-ups and quiet users every 5 minutes.
func (e *Engine) RunInitiative(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !e.Secrets.Has(ctx, "openrouter_api_key") {
				continue
			}
			e.FollowUps(ctx)
			e.CheckIns(ctx)
		}
	}
}

func (e *Engine) quiet(c *character.Character, u *store.User) bool {
	n := e.now().In(u.Location())
	return c.InQuietHours(n.Hour()*60 + n.Minute())
}

// FollowUps sends due follow-ups, one per user per run.
func (e *Engine) FollowUps(ctx context.Context) {
	due, err := e.DB.DueFollowups(ctx)
	if err != nil {
		e.Log.Error("due followups", "err", err)
		return
	}
	done := map[int64]bool{}
	for _, m := range due {
		if done[m.UserID] {
			continue
		}
		user, err := e.DB.UserByID(ctx, m.UserID)
		if err != nil {
			continue
		}
		char, ok := e.Chars.Get(m.Character)
		if !ok || !char.Initiative.FollowUps {
			if char, ok = e.Chars.Host(); !ok {
				continue
			}
		}
		if e.quiet(char, user) {
			continue // try again after quiet hours
		}
		done[m.UserID] = true
		// Expire first so a failure can't make it nag repeatedly.
		if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "expire", Memory: store.Memory{ID: m.ID}}}); err != nil {
			e.Log.Error("expire followup", "err", err)
			continue
		}
		e.proactive(ctx, user, char, fmt.Sprintf("Earlier you noted you'd follow up with the user about: %q. Send them a short, natural check-in about it now. One or two sentences.", m.Content))
	}
}

// CheckIns lets a sprig say hello after the user has been quiet a while.
func (e *Engine) CheckIns(ctx context.Context) {
	days := e.Settings.Int(ctx, "checkin_days")
	if days <= 0 {
		return
	}
	users, err := e.DB.ListUsers(ctx)
	if err != nil {
		return
	}
	window := time.Duration(days) * 24 * time.Hour
	for _, u := range users {
		u := u
		if u.OnboardedAt.IsZero() {
			continue
		}
		last, err := e.DB.LastUserMessageAt(ctx, u.ID)
		if err != nil || last.IsZero() || e.now().Sub(last) < window {
			continue
		}
		var char *character.Character
		for _, c := range e.Chars.All() {
			if c.Initiative.CheckIns && !e.quiet(c, &u) {
				char = c
				break
			}
		}
		if char == nil {
			continue
		}
		if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("checkin:%d", u.ID), window); !ok {
			continue
		}
		e.proactive(ctx, &u, char, fmt.Sprintf("The user hasn't talked to any sprig in %d days. Send a brief, warm, low-pressure check-in. If you remember something relevant (a plan, a loose end), mention it lightly. No guilt-tripping.", days))
	}
}

func (e *Engine) proactive(ctx context.Context, u *store.User, c *character.Character, event string) {
	t := &turn{user: u, char: c, query: event, noTools: true}
	pctx := llm.WithMeta(ctx, llm.Meta{UserID: u.ID, Character: c.ID, Purpose: "proactive"})
	text, err := e.runTurn(pctx, t, event)
	if err != nil {
		e.Log.Warn("proactive message failed", "user", u.ID, "character", c.ID, "err", err)
		return
	}
	if text == "" {
		return
	}
	if err := e.sendToUser(ctx, u, c, text); err != nil {
		e.Log.Error("send proactive message", "err", err)
		return
	}
	e.logActivity(ctx, "info", "character", c.ID, u.ID, "sent a check-in", truncate(text, 200))
}
