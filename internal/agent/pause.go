package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// The kill switch. Everything, or one sprig, can be paused: by the owner
// (everything) or a sprig's owner (that sprig), in chat apps' dashboard,
// the local API or `thicket pause` at the server. Pauses are stored, so
// they survive restarts, and go in the trail.
//
// Pausing stops what's in flight (replies, jobs and delegated tasks, the
// way "stop" does) and is checked, in code, at the start of every turn,
// tool call, job, trigger poll, delegated task and heartbeat, and under
// all of them on every model call. Messages that arrive get a short
// "paused" reply; nothing is queued for later.

// ErrPaused is the error of anything refused because of a pause.
var ErrPaused = errors.New("paused")

// pausedError says what is paused; sprig is the paused sprig's id, or ""
// for everything.
type pausedError struct{ msg, sprig string }

func (p *pausedError) Error() string { return p.msg }
func (p *pausedError) Unwrap() error { return ErrPaused }

// errStoppedByPause is the error of a job or task a pause cut short. It
// is an ErrStopped, so there's no failure report: whoever paused knows.
var errStoppedByPause error = stoppedBy("stopped by a pause")

type stoppedBy string

func (s stoppedBy) Error() string { return string(s) }
func (s stoppedBy) Unwrap() error { return ErrStopped }

// pauseWatchEvery is how often pauses made elsewhere (`thicket pause`
// writes the database) are picked up: well inside the promised second.
const pauseWatchEvery = 250 * time.Millisecond

// paused returns why a sprig (or, with "", everything) can't run now, or
// nil. A failed check counts as paused: the kill switch fails closed.
func (e *Engine) paused(ctx context.Context, charID string) error {
	p, ok, err := e.DB.PausedFor(context.WithoutCancel(ctx), charID)
	if err != nil {
		e.Log.Error("check pause; treating as paused", "err", err)
		return &pausedError{msg: "Thicket couldn't check whether it's paused, so nothing runs until it can"}
	}
	if !ok {
		return nil
	}
	if p.Scope == store.PauseAll {
		return &pausedError{msg: "Thicket is paused"}
	}
	return &pausedError{e.charName(p.Scope) + " is paused", p.Scope}
}

// Paused implements the budget guard's model-call check.
func (e *Engine) Paused(ctx context.Context, charID string) error { return e.paused(ctx, charID) }

// pausedReply is what a person is told when their message can't be
// answered because of a pause.
func pausedReply(err error) string {
	cmd := "`thicket resume`"
	var pe *pausedError
	if errors.As(err, &pe) && pe.sprig != "" {
		cmd = "`thicket resume --sprig " + pe.sprig + "`"
	}
	return "⏸️ " + err.Error() + ", so nothing is running. It resumes with " + cmd + ", from the dashboard, or through the API."
}

// mayPause checks who may pause or resume: everything, only the owner
// (or the server's operator, by nil); a sprig, its owner (see mayManage).
func (e *Engine) mayPause(ctx context.Context, by *store.User, c *character.Character, what string) error {
	if by == nil {
		return nil // the command line at the server
	}
	if c == nil {
		if by.Role == store.RoleOwner {
			return nil
		}
		e.refused(ctx, by, "", "tried to "+what+" everything")
		return errors.New("only the owner can " + what + " everything; you can " + what + " a sprig of your own")
	}
	return e.mayManage(ctx, by, c, what+" it")
}

func pauseScope(c *character.Character) string {
	if c == nil {
		return store.PauseAll
	}
	return c.ID
}

func (e *Engine) pauseLabel(c *character.Character) string {
	if c == nil {
		return "everything"
	}
	return c.DisplayName()
}

// Pause pauses everything (sprig "") or one sprig, and stops what it
// covers that's running now. by nil is the server's operator; how says
// where it came from ("from the command line"), for the trail.
func (e *Engine) Pause(ctx context.Context, by *store.User, sprig, how string) error {
	ctx = context.WithoutCancel(ctx)
	c, err := e.pauseTarget(ctx, by, sprig, "pause")
	if err != nil {
		return err
	}
	if err := e.mayPause(ctx, by, c, "pause"); err != nil {
		return err
	}
	added, err := e.DB.SetPause(ctx, pauseScope(c), userID(by), how)
	if err != nil {
		return err
	}
	e.HaltPaused(ctx)
	if added {
		e.addTrail(ctx, userID(by), pauseChar(c), store.TrailPause, "paused "+e.pauseLabel(c), how, nil)
		e.logActivity(ctx, "warn", "system", pauseChar(c), userID(by), "paused "+e.pauseLabel(c), how)
	}
	return nil
}

// Resume lifts a pause made with Pause. Resuming one sprig doesn't lift a
// pause of everything.
func (e *Engine) Resume(ctx context.Context, by *store.User, sprig, how string) error {
	ctx = context.WithoutCancel(ctx)
	c, err := e.pauseTarget(ctx, by, sprig, "resume")
	if err != nil {
		return err
	}
	if err := e.mayPause(ctx, by, c, "resume"); err != nil {
		return err
	}
	removed, err := e.DB.ClearPause(ctx, pauseScope(c))
	if err != nil {
		return err
	}
	if removed {
		e.addTrail(ctx, userID(by), pauseChar(c), store.TrailPause, "resumed "+e.pauseLabel(c), how, nil)
		e.logActivity(ctx, "info", "system", pauseChar(c), userID(by), "resumed "+e.pauseLabel(c), how)
	}
	return nil
}

func pauseChar(c *character.Character) string {
	if c == nil {
		return ""
	}
	return c.ID
}

// pauseTarget finds the sprig a pause is about ("" is everything).
// Someone else's private sprig isn't found, and the attempt is recorded
// without naming it.
func (e *Engine) pauseTarget(ctx context.Context, by *store.User, sprig, what string) (*character.Character, error) {
	if sprig == "" || sprig == store.PauseAll {
		return nil, nil
	}
	c, err := e.findSprig(nil, sprig)
	if err == nil && by != nil && c.PrivateFrom(by.ID) {
		e.refused(ctx, by, c.ID, "tried to "+what+" someone else's private sprig")
		err = fmt.Errorf("there's no sprig called %q", sprig)
	}
	return c, err
}

// PauseInfo is a pause as someone may see it.
type PauseInfo struct {
	Scope    string    `json:"scope"` // "*" or the sprig id
	Name     string    `json:"name"`  // "everything" or the sprig's name
	Reason   string    `json:"reason,omitempty"`
	PausedAt time.Time `json:"paused_at"`
}

// PausesFor lists the pauses in force that viewer may know about (another
// person's private sprig being paused is theirs to know). viewer nil sees
// everything.
func (e *Engine) PausesFor(ctx context.Context, viewer *store.User) ([]PauseInfo, error) {
	ps, err := e.DB.Pauses(ctx)
	if err != nil {
		return nil, err
	}
	out := []PauseInfo{}
	for _, p := range ps {
		in := PauseInfo{Scope: p.Scope, Name: "everything", Reason: p.Reason, PausedAt: p.PausedAt}
		if p.Scope != store.PauseAll {
			c, ok := e.Chars.Get(p.Scope)
			if viewer != nil && (!ok || c.PrivateFrom(viewer.ID)) {
				continue
			}
			in.Name = e.charName(p.Scope)
		}
		out = append(out, in)
	}
	return out, nil
}

// HaltPaused stops every reply, job and delegated task that a pause in
// force covers. It's called on every Pause, and by RunPauseWatch for
// pauses written by another process.
func (e *Engine) HaltPaused(ctx context.Context) {
	ps, err := e.DB.Pauses(context.WithoutCancel(ctx))
	if err != nil || len(ps) == 0 {
		return
	}
	all, chars := false, map[string]bool{}
	for _, p := range ps {
		if p.Scope == store.PauseAll {
			all = true
		}
		chars[p.Scope] = true
	}
	halt := func(_, v any) bool {
		f := v.(*inFlight)
		if (all || chars[f.charID()]) && !f.stopped.Load() && f.tryStop(func() { f.paused.Store(true) }) {
			f.cancel()
		}
		return true
	}
	e.turnsIn.Range(halt)
	e.jobsRunning.Range(halt)
}

// RunPauseWatch picks up pauses written by `thicket pause` until ctx ends.
func (e *Engine) RunPauseWatch(ctx context.Context) {
	t := time.NewTicker(pauseWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.HaltPaused(ctx)
		}
	}
}

func (e *Engine) toolPause(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig  string `json:"sprig"`
		Resume bool   `json:"resume"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.Resume {
		if a.Sprig == "" {
			return "", errors.New("to resume everything, use the dashboard, the API or `thicket resume`: while everything is paused, nobody here can answer")
		}
		if err := e.Resume(ctx, t.user, a.Sprig, "asked in chat"); err != nil {
			return "", err
		}
		return a.Sprig + " is resumed.", nil
	}
	if err := e.Pause(ctx, t.user, a.Sprig, "asked in chat"); err != nil {
		return "", err
	}
	if a.Sprig == "" {
		return "Everything is paused, you included: say so in one line, and that it resumes from the dashboard, the API or `thicket resume`.", nil
	}
	return a.Sprig + " is paused: it has stopped and won't run until resumed (ask me, or use the dashboard).", nil
}
