package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/theshield2594/thicket/internal/store"
)

// Stopping. Messages in a chat run one at a time, so a "stop" would wait
// behind the very reply it's meant to stop. Instead it's caught before
// the chat lock: it cancels the reply in progress (and any handoff it
// started), and messages sent before it that are still waiting are
// dropped. With no reply running, it stops the user's running jobs.

// ErrStopped is the error of a reply or job the user stopped.
var ErrStopped = errors.New("stopped by the user")

var stopRe = regexp.MustCompile(`(?i)^\s*(?:/?stop|cancel|abort|halt|never ?mind|nvm|stop it|stop that|please stop|🛑|✋|⏹️?)[\s.!]*$`)

func isStopRequest(text string) bool { return stopRe.MatchString(text) }

// inFlight is a reply or job that can be stopped.
type inFlight struct {
	userID  int64
	name    string // job name, for jobs
	cancel  context.CancelFunc
	stopped atomic.Bool
	acked   atomic.Bool
	dropped atomic.Int32 // queued messages set aside by the stop
}

type inFlightKey struct{}

func inFlightFrom(ctx context.Context) *inFlight {
	f, _ := ctx.Value(inFlightKey{}).(*inFlight)
	return f
}

// track registers work that can be stopped; done must be called after.
func (e *Engine) track(ctx context.Context, reg *sync.Map, key any, userID int64, name string) (context.Context, *inFlight, func()) {
	cctx, cancel := context.WithCancel(ctx)
	f := &inFlight{userID: userID, name: name, cancel: cancel}
	reg.Store(key, f)
	return context.WithValue(cctx, inFlightKey{}, f), f, func() {
		reg.CompareAndDelete(key, f)
		cancel()
	}
}

func (e *Engine) waiting(chatRef string) *atomic.Int32 {
	v, _ := e.waitingIn.LoadOrStore(chatRef, &atomic.Int32{})
	return v.(*atomic.Int32)
}

// Queued is how many messages in a chat are waiting for the reply in
// progress to finish.
func (e *Engine) Queued(chatRef string) int { return int(e.waiting(chatRef).Load()) }

// Stop stops the reply in progress in a chat, if the user started it or
// is an admin. Failing that, it stops the user's running jobs and returns
// their names. It never blocks or sends anything (the local API calls it
// while holding its own lock).
func (e *Engine) Stop(chatRef string, user *store.User) (stoppedReply bool, jobs []string) {
	if v, ok := e.turnsIn.Load(chatRef); ok {
		f := v.(*inFlight)
		if f.userID == user.ID || user.IsAdmin() {
			f.dropped.Store(e.waiting(chatRef).Load())
			e.stopSeq.Store(chatRef, e.arrivals.Load()) // everything already queued
			f.stopped.Store(true)
			f.cancel()
			return true, nil
		}
	}
	e.jobsRunning.Range(func(_, v any) bool {
		f := v.(*inFlight)
		if f.userID == user.ID && !f.stopped.Load() {
			f.stopped.Store(true)
			f.cancel()
			jobs = append(jobs, f.name)
		}
		return true
	})
	return false, jobs
}

// stopAck tells the user the reply stopped, what had already happened,
// and what was set aside. It runs on a context the stop didn't cancel.
func (e *Engine) stopAck(ctx context.Context, t *turn, f *inFlight) {
	if !f.acked.CompareAndSwap(false, true) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	msg := "Okay, stopped."
	if len(t.done) > 0 {
		msg += " Before you stopped me I had already done this: " + strings.Join(t.done, ", ") + "."
	}
	if n := f.dropped.Load(); n > 0 {
		msg += fmt.Sprintf(" I've also set aside the %d message(s) you sent while I was busy; send again anything you still want.", n)
	}
	if err := e.send(ctx, t.chatRef, t.char, msg); err != nil {
		e.Log.Error("send stop acknowledgement", "err", err)
	}
	if t.conv != nil {
		_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", "The user stopped the reply before it was finished.")
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "reply stopped by the user", strings.Join(t.done, ", "))
}
