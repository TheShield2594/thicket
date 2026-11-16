package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

type pausingExec struct {
	fakeExec
	paused string
}

func (p *pausingExec) PausedReason(context.Context, store.Job) string { return p.paused }

// The heartbeat-to-task handoff: a due job is handed to the executor once,
// and while spending is paused it is skipped, recorded, and not run.
func TestHeartbeatHandsDueJobsToExecutorUnlessPaused(t *testing.T) {
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	clock := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	db.SetClock(func() time.Time { return clock })
	u, _ := db.CreateUser(ctx, "discord", "1", "Sam", "owner")
	j, _ := db.CreateJob(ctx, store.Job{UserID: u.ID, Character: "juniper", Name: "morning", Schedule: "0 8 * * *", Prompt: "summary"})

	exec := &pausingExec{fakeExec: fakeExec{results: []Result{{Status: "ok", Output: "hi"}}}, paused: "the monthly spend cap ($30.00) is reached"}
	s := New(db, exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	day1 := time.Date(2026, 11, 10, 8, 0, 5, 0, time.UTC)
	s.Now = func() time.Time { return day1 }
	s.Tick(ctx)
	s.Wait()
	if len(exec.finished) != 0 {
		t.Fatal("a paused job ran")
	}
	runs, _ := db.JobRuns(ctx, j.ID, 5)
	if len(runs) != 1 || runs[0].Status != "missed" || !strings.HasPrefix(runs[0].Error, "skipped: the monthly spend cap") {
		t.Fatalf("skip not recorded: %+v", runs)
	}

	// Unpaused the next day: it runs once, and the paused day doesn't
	// count as missed.
	exec.paused = ""
	day2 := day1.Add(24 * time.Hour)
	s.Now = func() time.Time { return day2 }
	s.Tick(ctx)
	s.Wait()
	s.Tick(ctx)
	s.Wait()
	if len(exec.finished) != 1 || exec.finished[0].Output != "hi" {
		t.Fatalf("handoff: %+v", exec.finished)
	}
	runs, _ = db.JobRuns(ctx, j.ID, 5)
	if len(runs) != 2 || runs[0].Status != "ok" {
		t.Fatalf("runs after resuming: %+v", runs)
	}
}

func TestInterruptedRunsAreReportedAtStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, _ := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	u, _ := db.CreateUser(ctx, "discord", "1", "Sam", "owner")
	j, _ := db.CreateJob(ctx, store.Job{UserID: u.ID, Character: "juniper", Name: "morning", Schedule: "0 8 * * *", Prompt: "summary"})
	if _, err := db.StartJobRun(ctx, j.ID, time.Now()); err != nil { // left running by a crash
		t.Fatal(err)
	}
	exec := &fakeExec{}
	s := New(db, exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.failStale(ctx)
	if len(exec.finished) != 1 || !errors.Is(exec.finished[0].Err, ErrInterrupted) {
		t.Fatalf("interrupted run not reported: %+v", exec.finished)
	}
	runs, _ := db.JobRuns(ctx, j.ID, 5)
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].Error != store.StaleRunError {
		t.Fatalf("run not failed cleanly: %+v", runs)
	}
}
