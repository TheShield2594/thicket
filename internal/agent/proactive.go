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
	if char.IsKid() || user.IsKid() {
		return scheduler.Result{Err: fmt.Errorf("%s is a kid sprig (or this is a kid's job): kid sprigs don't run routines", char.DisplayName())}
	}
	t := &turn{user: user, char: char, job: &j, query: j.Name + " " + j.Prompt}
	ctx, flight, done := e.track(ctx, &e.jobsRunning, j.ID, user.ID, j.Name)
	defer done()
	jctx := llm.WithMeta(ctx, llm.Meta{UserID: user.ID, Character: char.ID, JobID: j.ID, Purpose: "job"})
	jctx, cancel := context.WithTimeout(jctx, 5*time.Minute)
	defer cancel()
	out, err := e.runTurn(jctx, t, "")
	cost, _ := e.DB.JobSpendSince(context.WithoutCancel(ctx), j.ID, start.Add(-time.Second))
	if flight.paused.Load() || errors.Is(err, ErrPaused) {
		return scheduler.Result{Err: errStoppedByPause, Cost: cost}
	}
	if flight.stopped.Load() {
		return scheduler.Result{Err: ErrStopped, Cost: cost}
	}
	if err != nil {
		return scheduler.Result{Err: err, Cost: cost}
	}
	if t.nothingToReport || out == "" {
		return scheduler.Result{Status: "empty", Cost: cost}
	}
	// The worth-saying gate (worth.go): output with nothing new in it, or a
	// rerun of what the sprig already said, is recorded as empty and never
	// sent. The run keeps it as its output, so it can be read.
	why := e.worthSaying(jctx, user, char, out, jobDraft(&j, t))
	cost, _ = e.DB.JobSpendSince(context.WithoutCancel(ctx), j.ID, start.Add(-time.Second))
	// The check can call a model: a stop or pause meanwhile still wins,
	// so nothing goes out after it and the run says why.
	if flight.paused.Load() {
		return scheduler.Result{Err: errStoppedByPause, Cost: cost}
	}
	if flight.stopped.Load() {
		return scheduler.Result{Err: ErrStopped, Cost: cost}
	}
	if why != "" {
		return scheduler.Result{Status: "empty", Output: out, Held: why, Cost: cost}
	}
	return scheduler.Result{Status: "ok", Output: out, Cost: cost}
}

// PausedReason holds scheduled jobs back while Thicket or the job's sprig
// is paused, or a spend cap is reached (scheduler.Pauser).
func (e *Engine) PausedReason(ctx context.Context, j store.Job) string {
	if err := e.paused(ctx, j.Character); err != nil {
		return err.Error()
	}
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
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q %s", j.ID, j.Name, res.Err), "")
		return
	}
	switch res.Status {
	case "ok":
		e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran", j.ID, j.Name), "")
		// Job and trigger output is proactive: quiet hours and the daily
		// budget may hold it for later (see gate.go).
		if _, err := e.deliverProactive(ctx, user, char, res.Output, fmt.Sprintf("job:%d", j.ID), fmt.Sprintf("job “%s”", j.Name)); err != nil {
			e.Log.Error("deliver job output", "job", j.ID, "err", err)
			e.logActivity(ctx, "error", "job", char.ID, user.ID, fmt.Sprintf("job #%d ran but its message couldn't be delivered", j.ID), err.Error())
			e.deadLetter(ctx, store.InboxItem{UserID: user.ID, Character: char.ID, Kind: store.InboxUndelivered, Ref: fmt.Sprintf("job:%d", j.ID),
				Title: fmt.Sprintf("Job “%s” ran, but its message couldn't be delivered", j.Name), Detail: res.Output})
		}
	case "empty":
		if res.Held != "" {
			e.logActivity(ctx, "info", "job", char.ID, user.ID, fmt.Sprintf("job #%d %q ran: held back, %s", j.ID, j.Name, res.Held), truncate(res.Output, 200))
			return
		}
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
		msg += e.mustReach(ctx, char, user, "a failed job needs you")
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
		msg += e.mustReach(ctx, char, user, "an interrupted job may need you")
		if err := e.sendToUser(ctx, user, char, msg); err != nil {
			e.Log.Error("tell the user about an interrupted job", "job", j.ID, "err", err) // it's in the inbox regardless
		}
	}
	return nil
}

// deadLetter puts something that needs a person in the inbox. If even that
// fails, the log is the last resort.
func (e *Engine) deadLetter(ctx context.Context, it store.InboxItem) {
	_ = e.deadLetterErr(ctx, it)
}

// deadLetterErr is deadLetter for callers that keep the item elsewhere
// when the inbox can't take it.
func (e *Engine) deadLetterErr(ctx context.Context, it store.InboxItem) error {
	it.Detail = e.Secrets.Redact(truncate(it.Detail, 4000))
	if _, err := e.DB.AddInbox(context.WithoutCancel(ctx), it); err != nil {
		e.Log.Error("dead-letter to inbox failed", "title", it.Title, "err", err)
		return err
	}
	return nil
}

// --- light initiative: follow-ups and check-ins ---

// RunInitiative delivers held messages, sends weekly digests, and checks
// for due follow-ups and quiet users every 5 minutes.
func (e *Engine) RunInitiative(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Paused: nothing goes out, not even held messages; they wait.
			if e.paused(ctx, "") != nil {
				continue
			}
			// Held messages are already written: no model needed.
			e.FlushHeld(ctx)
			// Nor is the weekly digest, which is built from the database.
			e.WeeklyDigests(ctx)
			if !e.ModelReady(ctx) || e.Budget.HeartbeatPause(ctx, "") != "" {
				continue // follow-ups wait; nothing is lost
			}
			e.FollowUps(ctx)
			e.CheckIns(ctx)
			e.Presets(ctx) // morning briefs, event heads-ups, due reminders
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
		if user.IsKid() {
			expire("kids don't get follow-ups")
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
		if e.holdReason(ctx, char, user) != "" || e.paused(ctx, char.ID) != nil {
			continue // try again after quiet hours, with tomorrow's budget, or once resumed
		}
		if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("followup:%d", m.ID), time.Hour); !ok {
			continue // attempted recently
		}
		done[m.UserID] = true
		err = e.proactive(ctx, user, char, fmt.Sprintf("Earlier you noted you'd follow up with the user about: %q. Send them a short, natural check-in about it now. One or two sentences.", m.Content),
			proactiveDraft{what: "a follow-up", news: []string{"a follow-up they noted is due: " + truncate(m.Content, 200)}})
		if err == nil || errors.Is(err, errNotWorth) {
			expire("") // handled: sent, or held back as saying nothing new (the log says why)
		} else if errors.Is(err, errHeld) || errors.Is(err, ErrPaused) || errors.Is(err, ErrStopped) {
			_ = e.DB.ReleaseNotice(ctx, fmt.Sprintf("followup:%d", m.ID)) // retry as soon as it may go out
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
		// Admins mid-setup aren't nudged; others have no setup tour to finish.
		if u.OnboardedAt.IsZero() && u.IsAdmin() {
			continue
		}
		last, err := e.DB.LastUserMessageAt(ctx, u.ID)
		if err != nil || last.IsZero() || e.now().Sub(last) < window {
			continue
		}
		var char *character.Character
		for _, c := range e.Chars.For(&u) {
			if c.Initiative.CheckIns && !c.IsKid() && e.holdReason(ctx, c, &u) == "" && e.paused(ctx, c.ID) == nil {
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
		err = e.proactive(ctx, &u, char, fmt.Sprintf("The user hasn't talked to any sprig in %d days. Send a brief, warm, low-pressure check-in. If you remember something relevant (a plan, a loose end), mention it lightly. No guilt-tripping.", days),
			proactiveDraft{what: "a check-in"})
		// Held back by the worth-saying gate: the claim stays, so this quiet
		// spell gets no other check-in.
		if errors.Is(err, errHeld) || errors.Is(err, ErrPaused) || errors.Is(err, ErrStopped) {
			// Not sent: free the claim so it goes out once it may.
			_ = e.DB.ReleaseNotice(ctx, key)
		}
	}
}

// errHeld means generation finished when the gate no longer let the
// message out (quiet hours started, or the budget was used meanwhile), so
// nothing was sent.
var errHeld = errors.New("held by quiet hours or the daily budget; not sent")

// proactive generates and delivers a message the user didn't ask for. It
// returns nil only if the message was delivered, and errNotWorth if the
// worth-saying gate (worth.go) held it back. Callers check the hold gate
// first, so no generation is wasted; nothing is stored here, because what
// the message is about stays due and is simply tried again.
func (e *Engine) proactive(ctx context.Context, u *store.User, c *character.Character, event string, d proactiveDraft) error {
	t := &turn{user: u, char: c, query: event, noTools: true}
	// Tracked like a job, so pausing the sprig cancels the generation.
	ctx, flight, done := e.track(ctx, &e.jobsRunning, fmt.Sprintf("proactive:%d", e.taskSeq.Add(1)), u.ID, "a check-in")
	defer done()
	flight.setChar(c.ID)
	pctx := llm.WithMeta(ctx, llm.Meta{UserID: u.ID, Character: c.ID, Purpose: "proactive"})
	text, err := e.runTurn(pctx, t, event)
	if flight.paused.Load() {
		return errStoppedByPause
	}
	if err != nil {
		e.Log.Warn("proactive message failed", "user", u.ID, "character", c.ID, "err", err)
		return err
	}
	if text == "" {
		return errors.New("model produced no message")
	}
	if why := e.worthSaying(pctx, u, c, text, d); why != "" {
		e.logActivity(ctx, "info", "initiative", c.ID, u.ID, "held back "+d.what+": "+why, truncate(text, 200))
		return errNotWorth
	}
	// Generation can take a while: re-check the gate right before sending.
	if e.holdReason(ctx, c, u) != "" {
		return errHeld
	}
	if err := e.paused(ctx, c.ID); err != nil {
		return err
	}
	if err := e.sendProactive(ctx, u, c, text); err != nil {
		e.Log.Error("send proactive message", "err", err)
		return err
	}
	e.logActivity(ctx, "info", "character", c.ID, u.ID, "sent "+d.what, truncate(text, 200))
	return nil
}
