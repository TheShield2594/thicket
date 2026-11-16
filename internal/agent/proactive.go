package agent

import (
	"context"
	"errors"
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
	ctx, flight, done := e.track(ctx, &e.jobsRunning, j.ID, user.ID, j.Name)
	defer done()
	jctx := llm.WithMeta(ctx, llm.Meta{UserID: user.ID, Character: char.ID, JobID: j.ID, Purpose: "job"})
	jctx, cancel := context.WithTimeout(jctx, 5*time.Minute)
	defer cancel()
	out, err := e.runTurn(jctx, t, "")
	cost, _ := e.DB.JobSpendSince(context.WithoutCancel(ctx), j.ID, start.Add(-time.Second))
	if flight.stopped.Load() {
		return scheduler.Result{Err: ErrStopped, Cost: cost}
	}
	if err != nil {
		return scheduler.Result{Err: err, Cost: cost}
	}
	if t.nothingToReport || out == "" {
		return scheduler.Result{Status: "empty", Cost: cost}
	}
	return scheduler.Result{Status: "ok", Output: out, Cost: cost}
}

// PausedReason holds scheduled jobs back while a spend cap is reached
// (scheduler.Pauser).
func (e *Engine) PausedReason(ctx context.Context, j store.Job) string {
	why := e.Budget.HeartbeatPause(ctx, j.Character)
	if why != "" {
		e.logActivity(ctx, "warn", "budget", j.Character, j.UserID, fmt.Sprintf("job #%d %q skipped: %s", j.ID, j.Name, why), "")
	}
	return why
}

// JobFinished delivers a job's result, or reports its failure loudly.
func (e *Engine) JobFinished(ctx context.Context, j store.Job, res scheduler.Result, next time.Time) {
	// The lookup must work during shutdown too, so failures still reach
	// the inbox (delivery itself stays cancellable).
	user, err := e.DB.UserByID(context.WithoutCancel(ctx), j.UserID)
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
	if errors.Is(res.Err, ErrStopped) {
		// The user asked for it and was told: no failure report.
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q stopped by the user", j.ID, j.Name), "")
		return
	}
	switch res.Status {
	case "ok":
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran", j.ID, j.Name), "")
		if err := e.sendToUser(ctx, user, char, res.Output); err != nil {
			e.Log.Error("deliver job output", "job", j.ID, "err", err)
			e.logActivity(ctx, "error", "job", char.ID, user.ID, fmt.Sprintf("job #%d ran but its message couldn't be delivered", j.ID), err.Error())
			e.deadLetter(ctx, store.InboxItem{UserID: user.ID, Character: char.ID, Kind: store.InboxUndelivered, Ref: fmt.Sprintf("job:%d", j.ID),
				Title: fmt.Sprintf("Job “%s” ran, but its message couldn't be delivered", j.Name), Detail: res.Output})
		}
	case "empty":
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran: nothing to report", j.ID, j.Name), "")
	default:
		errText := "unknown error"
		if res.Err != nil {
			errText = res.Err.Error()
		}
		e.logActivity(ctx, "error", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q failed", j.ID, j.Name), errText)
		// Every failure lands in the inbox (repeats bump one item), even
		// when the chat report below is held back as a duplicate.
		kind, title := store.InboxFailed, fmt.Sprintf("Job “%s” failed", j.Name)
		if errors.Is(res.Err, scheduler.ErrInterrupted) {
			kind, title = store.InboxInterrupted, fmt.Sprintf("Job “%s” was interrupted by a restart", j.Name)
		}
		e.deadLetter(ctx, store.InboxItem{UserID: user.ID, Character: char.ID, Kind: kind, Ref: fmt.Sprintf("job:%d", j.ID), Title: title, Detail: errText})
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

// ReportInterruptedRun records a job run a restart cut short in the inbox and
// tells its owner (scheduler.InterruptReporter). It fails if the inbox
// item can't be stored, so the run is reported again at the next start;
// the chat message is best effort.
func (e *Engine) ReportInterruptedRun(ctx context.Context, j store.Job) error {
	ctx = context.WithoutCancel(ctx)
	user, err := e.DB.UserByID(ctx, j.UserID)
	if err != nil {
		return fmt.Errorf("look up the job's owner: %w", err)
	}
	if _, err := e.DB.AddInbox(ctx, store.InboxItem{UserID: user.ID, Character: j.Character, Kind: store.InboxInterrupted,
		Ref: fmt.Sprintf("job:%d", j.ID), Title: fmt.Sprintf("Job “%s” was interrupted by a restart", j.Name), Detail: store.StaleRunError}); err != nil {
		return fmt.Errorf("store the report: %w", err)
	}
	e.logActivity(ctx, "error", "job", j.Character, user.ID, fmt.Sprintf("job #%d %q was interrupted by a restart", j.ID, j.Name), "")
	char, ok := e.Chars.Get(j.Character)
	if !ok {
		char, ok = e.Chars.Host()
	}
	if ok {
		msg := fmt.Sprintf("⚠️ Thicket restarted while my job “%s” was running, so it didn't finish. It'll run again at its next time.", j.Name)
		if err := e.sendToUser(ctx, user, char, msg); err != nil {
			e.Log.Error("tell the user about an interrupted job", "job", j.ID, "err", err) // it's in the inbox regardless
		}
	}
	return nil
}

// deadLetter puts something that needs a person in the inbox. If even that
// fails, the log is the last resort.
func (e *Engine) deadLetter(ctx context.Context, it store.InboxItem) {
	it.Detail = e.Secrets.Redact(truncate(it.Detail, 4000))
	if _, err := e.DB.AddInbox(context.WithoutCancel(ctx), it); err != nil {
		e.Log.Error("dead-letter to inbox failed", "title", it.Title, "err", err)
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
			if !e.ModelReady(ctx) || e.Budget.HeartbeatPause(ctx, "") != "" {
				continue // follow-ups wait; nothing is lost
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

// FollowUps sends due follow-ups, one per user per run. A follow-up is
// only marked done once it was actually delivered; a failed attempt is
// retried after an hour (the claim stops duplicate sends meanwhile).
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
		expire := func(why string) {
			if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "expire", Memory: store.Memory{ID: m.ID}}}); err != nil {
				e.Log.Error("expire followup", "err", err)
			}
			if why != "" {
				e.logActivity(ctx, "info", "character", m.Character, m.UserID, "follow-up dropped: "+why, m.Content)
			}
		}
		if e.now().Sub(m.DueAt) > 3*24*time.Hour {
			expire("it was more than 3 days overdue")
			continue
		}
		char, ok := e.Chars.Get(m.Character)
		if !ok || !char.Initiative.FollowUps {
			char, ok = e.Chars.Host()
		}
		// The final choice must allow follow-ups too, host included.
		if !ok || !char.Initiative.FollowUps {
			expire("neither the sprig that noted it nor the host has follow-ups switched on")
			continue
		}
		if e.quiet(char, user) {
			continue // try again after quiet hours
		}
		if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("followup:%d", m.ID), time.Hour); !ok {
			continue // attempted recently
		}
		done[m.UserID] = true
		err = e.proactive(ctx, user, char, fmt.Sprintf("Earlier you noted you'd follow up with the user about: %q. Send them a short, natural check-in about it now. One or two sentences.", m.Content))
		if err == nil {
			expire("")
		}
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
		// One check-in per quiet spell: the key changes only when the user
		// writes again, so a silent user isn't pinged every N days.
		key := fmt.Sprintf("checkin:%d:%d", u.ID, last.Unix())
		if ok, _ := e.DB.ClaimOnce(ctx, key); !ok {
			continue
		}
		err = e.proactive(ctx, &u, char, fmt.Sprintf("The user hasn't talked to any sprig in %d days. Send a brief, warm, low-pressure check-in. If you remember something relevant (a plan, a loose end), mention it lightly. No guilt-tripping.", days))
		if errors.Is(err, errQuietHours) {
			// Not sent: free the claim so it goes out after quiet hours.
			_ = e.DB.ReleaseNotice(ctx, key)
		}
	}
}

// errQuietHours means generation finished inside quiet hours, so nothing
// was sent.
var errQuietHours = errors.New("quiet hours started; not sent")

// proactive generates and delivers a message the user didn't ask for. It
// returns nil only if the message was delivered.
func (e *Engine) proactive(ctx context.Context, u *store.User, c *character.Character, event string) error {
	t := &turn{user: u, char: c, query: event, noTools: true}
	pctx := llm.WithMeta(ctx, llm.Meta{UserID: u.ID, Character: c.ID, Purpose: "proactive"})
	text, err := e.runTurn(pctx, t, event)
	if err != nil {
		e.Log.Warn("proactive message failed", "user", u.ID, "character", c.ID, "err", err)
		return err
	}
	if text == "" {
		return errors.New("model produced no message")
	}
	// Generation can take a while: re-check quiet hours right before sending.
	if e.quiet(c, u) {
		return errQuietHours
	}
	if err := e.sendToUser(ctx, u, c, text); err != nil {
		e.Log.Error("send proactive message", "err", err)
		return err
	}
	e.logActivity(ctx, "info", "character", c.ID, u.ID, "sent a check-in", truncate(text, 200))
	return nil
}
