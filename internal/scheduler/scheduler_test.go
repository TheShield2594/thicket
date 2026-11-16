package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

func TestParse(t *testing.T) {
	for _, ok := range []string{"0 8 * * *", "30 7 * * 1-5", "@daily", "@every 1h", "*/15 * * * *"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"* * * * *", "*/5 * * * *", "@every 1m", "nonsense", "0 8 * *", "CRON_TZ=UTC 0 8 * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestDue(t *testing.T) {
	s, _ := Parse("0 8 * * *")
	created := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	j := store.Job{CreatedAt: created}

	if due, _ := Due(j, s, time.Date(2026, 11, 11, 7, 59, 0, 0, time.UTC), time.UTC); due {
		t.Fatal("not due before 8:00")
	}
	if due, missed := Due(j, s, time.Date(2026, 11, 11, 8, 0, 30, 0, time.UTC), time.UTC); !due || missed != 0 {
		t.Fatalf("due=%v missed=%d", due, missed)
	}
	// Down for three days: one run now, two recorded as missed.
	if due, missed := Due(j, s, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), time.UTC); !due || missed != 2 {
		t.Fatalf("due=%v missed=%d", due, missed)
	}
	j.LastRunAt = time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	if due, _ := Due(j, s, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), time.UTC); due {
		t.Fatal("already ran today")
	}
	// Timezones: 8:00 in New York is 13:00 UTC in November.
	ny, _ := time.LoadLocation("America/New_York")
	j.LastRunAt = time.Time{}
	j.CreatedAt = time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC) // midnight in New York
	if due, _ := Due(j, s, time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC), ny); due {
		t.Fatal("not yet 8:00 in New York")
	}
	if due, _ := Due(j, s, time.Date(2026, 11, 11, 13, 1, 0, 0, time.UTC), ny); !due {
		t.Fatal("should be due at 8:00 New York")
	}
}

type fakeExec struct {
	mu       sync.Mutex
	results  []Result
	finished []Result
}

func (f *fakeExec) RunJob(ctx context.Context, j store.Job, runID int64) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.results[0]
	f.results = f.results[1:]
	return r
}
func (f *fakeExec) JobFinished(ctx context.Context, j store.Job, res Result, next time.Time) {
	f.mu.Lock()
	f.finished = append(f.finished, res)
	f.mu.Unlock()
}
func (f *fakeExec) Location(context.Context, int64) *time.Location { return time.UTC }

func TestTickRecordsEveryOutcome(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	db.SetClock(func() time.Time { return clock })
	u, _ := db.CreateUser(ctx, "discord", "1", "Sam", "owner")
	j, _ := db.CreateJob(ctx, store.Job{UserID: u.ID, Character: "juniper", Name: "morning", Schedule: "0 8 * * *", Prompt: "summary"})

	exec := &fakeExec{results: []Result{
		{Status: "ok", Output: "Good morning!"},
		{Status: "empty"},
		{Err: errors.New("calendar API down")},
	}}
	s := New(db, exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for day := 0; day < 3; day++ {
		now := time.Date(2026, 11, 10+day, 8, 0, 5, 0, time.UTC)
		clock = now
		s.Now = func() time.Time { return now }
		s.Tick(ctx)
		s.wg.Wait()
		s.Tick(ctx) // a second tick the same minute must not re-run
		s.wg.Wait()
	}
	runs, err := db.JobRuns(ctx, j.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("want 3 runs, got %d", len(runs))
	}
	// Newest first.
	if runs[0].Status != "error" || runs[0].Error != "calendar API down" || runs[1].Status != "empty" || runs[2].Status != "ok" || runs[2].Output != "Good morning!" {
		t.Fatalf("unexpected runs: %+v", runs)
	}
	if len(exec.finished) != 3 {
		t.Fatalf("JobFinished called %d times", len(exec.finished))
	}
}

func TestInvalidScheduleIsPausedLoudly(t *testing.T) {
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	u, _ := db.CreateUser(ctx, "discord", "1", "Sam", "owner")
	j, _ := db.CreateJob(ctx, store.Job{UserID: u.ID, Character: "juniper", Name: "bad", Schedule: "* * * * *", Prompt: "x"})
	exec := &fakeExec{}
	s := New(db, exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Tick(ctx)
	got, _ := db.JobByID(ctx, j.ID)
	if got.Enabled {
		t.Fatal("job with invalid schedule should be paused")
	}
	if len(exec.finished) != 1 || exec.finished[0].Status != "error" {
		t.Fatalf("failure not reported: %+v", exec.finished)
	}
}
