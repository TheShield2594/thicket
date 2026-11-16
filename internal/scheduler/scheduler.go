// Package scheduler runs cron-style jobs owned by characters and records
// every run, so "nothing found" is distinguishable from "silently broke".
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/theshield2594/thicket/internal/store"
)

// MinInterval stops a typo like "* * * * *" from burning the budget.
const MinInterval = 15 * time.Minute

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a schedule ("0 8 * * 1-5", "@daily", "@every 2h").
func Parse(spec string) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {
		return nil, fmt.Errorf("don't put a timezone in the schedule; jobs use the user's timezone")
	}
	s, err := parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %v (use 5 fields: minute hour day month weekday, e.g. \"0 8 * * *\")", spec, err)
	}
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	a := s.Next(t0)
	b := s.Next(a)
	if a.IsZero() || b.IsZero() {
		return nil, fmt.Errorf("schedule %q never fires", spec)
	}
	// Check the tightest gap over a day of firings.
	prev := a
	for i := 0; i < 100; i++ {
		n := s.Next(prev)
		if n.Sub(prev) < MinInterval {
			return nil, fmt.Errorf("schedule %q fires more often than every %s", spec, MinInterval)
		}
		prev = n
	}
	return s, nil
}

// Describe returns the next few run times for a confirmation message.
func Describe(spec string, loc *time.Location, from time.Time, n int) ([]time.Time, error) {
	s, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	var out []time.Time
	t := from.In(loc)
	for i := 0; i < n; i++ {
		t = s.Next(t)
		out = append(out, t)
	}
	return out, nil
}

// Due reports whether a job should run at now, and how many scheduled runs
// beyond the first were missed (e.g. while Thicket was down).
func Due(j store.Job, sched cron.Schedule, now time.Time, loc *time.Location) (due bool, missed int) {
	base := j.LastRunAt
	if base.IsZero() {
		base = j.CreatedAt
	}
	next := sched.Next(base.In(loc))
	if next.After(now) {
		return false, 0
	}
	for t := sched.Next(next); !t.After(now) && missed < 1000; t = sched.Next(t) {
		missed++
	}
	return true, missed
}

// Next returns the next run time after now.
func Next(spec string, now time.Time, loc *time.Location) time.Time {
	s, err := Parse(spec)
	if err != nil {
		return time.Time{}
	}
	return s.Next(now.In(loc))
}

type Result struct {
	Status string // ok | empty | error
	Output string
	Err    error
	Cost   float64
}

type Executor interface {
	RunJob(ctx context.Context, job store.Job, runID int64) Result
	JobFinished(ctx context.Context, job store.Job, res Result, next time.Time)
	Location(ctx context.Context, userID int64) *time.Location
}

type Scheduler struct {
	DB   *store.Store
	Exec Executor
	Log  *slog.Logger
	Now  func() time.Time

	mu       sync.Mutex
	running  map[int64]bool
	lastTick time.Time
	wg       sync.WaitGroup
}

func New(db *store.Store, exec Executor, log *slog.Logger) *Scheduler {
	return &Scheduler{DB: db, Exec: exec, Log: log, Now: time.Now, running: map[int64]bool{}}
}

// LastTick is when the scheduler last checked for due jobs (for /health).
func (s *Scheduler) LastTick() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTick
}

// Run checks for due jobs every 20 seconds until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	if n, err := s.DB.FailStaleRuns(ctx); err != nil {
		s.Log.Error("fail stale runs", "err", err)
	} else if n > 0 {
		s.Log.Warn("marked interrupted job runs as failed", "count", n)
	}
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs every job that is due. Exposed for tests.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.Now()
	s.mu.Lock()
	s.lastTick = now
	s.mu.Unlock()
	jobs, err := s.DB.EnabledJobs(ctx)
	if err != nil {
		s.Log.Error("list jobs", "err", err)
		return
	}
	for _, j := range jobs {
		sched, err := Parse(j.Schedule)
		if err != nil {
			s.Log.Error("job has invalid schedule; disabling", "job", j.ID, "err", err)
			_ = s.DB.SetJobEnabled(ctx, j.ID, false)
			s.Exec.JobFinished(ctx, j, Result{Status: "error", Err: fmt.Errorf("schedule is invalid, so I paused the job: %w", err)}, time.Time{})
			continue
		}
		loc := s.Exec.Location(ctx, j.UserID)
		due, missed := Due(j, sched, now, loc)
		if !due {
			continue
		}
		s.mu.Lock()
		if s.running[j.ID] {
			s.mu.Unlock()
			continue
		}
		s.running[j.ID] = true
		s.mu.Unlock()
		if missed > 0 {
			_ = s.DB.RecordMissedRun(ctx, j.ID, missed, now)
		}
		s.wg.Add(1)
		go func(j store.Job) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.running, j.ID)
				s.mu.Unlock()
			}()
			s.runOne(ctx, j, now, sched, loc)
		}(j)
	}
}

func (s *Scheduler) runOne(ctx context.Context, j store.Job, now time.Time, sched cron.Schedule, loc *time.Location) {
	runID, err := s.DB.StartJobRun(ctx, j.ID, now)
	if err != nil {
		s.Log.Error("start job run", "job", j.ID, "err", err)
		return
	}
	var res Result
	func() {
		defer func() {
			if p := recover(); p != nil {
				res = Result{Status: "error", Err: fmt.Errorf("job crashed: %v", p)}
			}
		}()
		res = s.Exec.RunJob(ctx, j, runID)
	}()
	if res.Status == "" {
		res.Status = "ok"
	}
	if res.Err != nil {
		res.Status = "error"
	}
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	if err := s.DB.FinishJobRun(ctx, runID, res.Status, res.Output, errText, res.Cost); err != nil {
		s.Log.Error("finish job run", "job", j.ID, "err", err)
	}
	s.Log.Info("job finished", "job", j.ID, "name", j.Name, "status", res.Status)
	// Same base Due uses (last_run_at = this tick), so the reported next
	// run matches when it will actually fire.
	s.Exec.JobFinished(ctx, j, res, sched.Next(now.In(loc)))
}
