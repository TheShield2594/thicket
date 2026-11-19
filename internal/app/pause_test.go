package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
)

// reopen starts a second App on the same data, as a restart would.
func reopen(t *testing.T, a *App, fc *fakeChannel, fm *fakeModel) *App {
	t.Helper()
	a.Close()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir, cfg.CharactersDir, cfg.HTTPAddr = a.Cfg.DataDir, a.Cfg.CharactersDir, "127.0.0.1:0"
	b, err := New(context.Background(), cfg, "test", Options{
		Model:    fm,
		KeyCheck: func(context.Context, string) error { return nil },
		Channels: func(*slog.Logger) []channel.Channel { return []channel.Channel{fc} },
		LogOut:   io.Discard,
		NoHTTP:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

// The issue's bar: `thicket pause` during a running job stops it within a
// second, and nothing runs until `resume`, across a restart.
func TestPauseStopsAJobAndHoldsAcrossRestart(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Big report", Kind: store.JobWebhook, HookHash: "h", Prompt: "Do the slow task."})
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	fm.mu.Unlock()
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go a.Engine.RunPauseWatch(watch)
	if err := a.Scheduler.Fire(*j); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}

	// What `thicket pause` does: write the pause into the database, from
	// outside the server. The running server notices and stops the job.
	if _, err := a.DB.SetPause(ctx, store.PauseAll, 0, "from the command line"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan struct{})
	go func() { a.Scheduler.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the job was still running a second after the pause")
	}
	t.Logf("job stopped %v after the pause", time.Since(start).Round(time.Millisecond))
	runs, _ := a.DB.JobRuns(ctx, j.ID, 1)
	if len(runs) != 1 || runs[0].Error != "stopped by a pause" {
		t.Fatalf("run history: %+v", runs)
	}
	if out := sentText(fc.take()); strings.Contains(out, "didn't work") || strings.Contains(out, "Finished") {
		t.Fatalf("a paused job reported:\n%s", out)
	}

	// Paused: messages get a short answer from code, no model is called,
	// and jobs and webhooks don't start.
	calls := func() int { fm.mu.Lock(); defer fm.mu.Unlock(); return fm.calls }
	before := calls()
	a.Engine.HandleIncoming(ctx, dm("hello?"))
	if out := sentText(fc.take()); !strings.Contains(out, "Thicket is paused") {
		t.Fatalf("message while paused:\n%s", out)
	}
	if err := a.Scheduler.Fire(*j); !errors.Is(err, scheduler.ErrHeld) || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("webhook while paused: %v", err)
	}
	if calls() != before {
		t.Fatal("a model was called while paused")
	}

	// A restart doesn't lift it.
	stopWatch()
	b := reopen(t, a, fc, fm)
	b.Engine.HandleIncoming(ctx, dm("still there?"))
	if out := sentText(fc.take()); !strings.Contains(out, "Thicket is paused") {
		t.Fatalf("message after a restart:\n%s", out)
	}
	if calls() != before {
		t.Fatal("a model was called after the restart, while paused")
	}

	// Only the owner resumes everything; then things run again.
	if err := b.Engine.Resume(ctx, owner, "", "test"); err != nil {
		t.Fatal(err)
	}
	b.Engine.HandleIncoming(ctx, dm("hello again"))
	if out := sentText(fc.take()); strings.Contains(out, "paused") || !strings.Contains(out, "Juni") {
		t.Fatalf("after resume:\n%s", out)
	}
	trail, _ := b.DB.Trail(ctx, 0, 20)
	if !trailHas(trail, store.TrailPause, "resumed everything") {
		t.Fatalf("resume isn't in the trail: %+v", trail)
	}
}

func trailHas(entries []store.TrailEntry, kind, summary string) bool {
	for _, e := range entries {
		if e.Kind == kind && strings.Contains(e.Summary, summary) {
			return true
		}
	}
	return false
}

// Pausing a reply in progress stops it, and says it was a pause.
func TestPauseStopsAReply(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	done := startSlow(t, a, fm, ctx, dm("now the slow task"))
	if err := a.Engine.Pause(ctx, owner, "juniper", "test"); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	if out := texts(fc.take()); !strings.Contains(out, "Juni is paused, so I've stopped") {
		t.Fatalf("reply stopped by a pause:\n%s", out)
	}
	// Only Juni is paused: messages to her get the short answer.
	a.Engine.HandleIncoming(ctx, dm("are you there"))
	if out := texts(fc.take()); !strings.Contains(out, "Juni is paused") {
		t.Fatalf("message to a paused sprig:\n%s", out)
	}
	if err := a.Engine.Resume(ctx, owner, "juniper", "test"); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, dm("are you there"))
	if out := texts(fc.take()); strings.Contains(out, "paused") {
		t.Fatalf("after resume:\n%s", out)
	}
}

// Who may pause what: the owner everything, a sprig's owner that sprig.
func TestWhoMayPause(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	if err := a.Engine.Pause(ctx, h.kim, "", "test"); err == nil {
		t.Fatal("a member paused everything")
	}
	if err := a.Engine.Pause(ctx, h.kim, "hal", "test"); err == nil {
		t.Fatal("a member paused a family sprig")
	}
	trail, _ := a.DB.Trail(ctx, 0, 20)
	if !trailHas(trail, store.TrailRefused, "tried to pause everything") || !trailHas(trail, store.TrailRefused, "tried to pause it on Hal") {
		t.Fatalf("refusals aren't in the trail: %+v", trail)
	}
	// Kim's own sprig is hers to pause, and not the owner's.
	writeSolo(t, a, "wren", h.kim.ID)
	if err := a.Engine.Pause(ctx, h.owner, "wren", "test"); err == nil {
		t.Fatal("the owner paused someone else's private sprig")
	}
	if err := a.Engine.Pause(ctx, h.kim, "wren", "test"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := a.Engine.PausesFor(ctx, h.owner); len(ps) != 0 {
		t.Fatalf("the owner can see a private sprig's pause: %+v", ps)
	}
	if ps, _ := a.Engine.PausesFor(ctx, h.kim); len(ps) != 1 || ps[0].Name != "Wren" {
		t.Fatalf("Kim's pauses: %+v", ps)
	}
	if err := a.Engine.Pause(ctx, h.owner, "", "test"); err != nil {
		t.Fatal(err)
	}
}

func writeSolo(t *testing.T, a *App, id string, owner int64) {
	t.Helper()
	src := "---\nid: " + id + "\nname: " + strings.ToUpper(id[:1]) + id[1:] + "\nrole: Keeps a reading list.\nbelongs_to: " + fmt.Sprint(owner) +
		"\npersonality:\n  traits: [quiet]\nvoice:\n  style: [short]\n---\nA private sprig.\n"
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, id+".md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
}

// Rate limits: per sprig (tool calls, actions without asking) and per
// integration; a call over one isn't run and the owner hears in the inbox.
func TestRateLimits(t *testing.T) {
	a, fc, _, ctx, reads, writes := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if err := a.Settings.Set(ctx, "limits.sprig.hal.tool_calls_per_minute", "2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a.Engine.HandleIncoming(ctx, dm("hal, check the light"))
	}
	fc.take()
	if reads.Load() != 2 {
		t.Fatalf("%d reads with a limit of 2 a minute", reads.Load())
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || !strings.Contains(items[0].Title, "Hal hit a rate limit: 2 tool calls a minute") {
		t.Fatalf("inbox: %+v", items)
	}

	// Actions without asking have their own, hourly, limit.
	_ = a.Settings.Set(ctx, "limits.sprig.hal.tool_calls_per_minute", "0")
	_ = a.Settings.Set(ctx, "limits.sprig.hal.actions_per_hour", "1")
	if err := a.Engine.SetToolLevel(ctx, owner, "hal", "mcp_ha__turn_on", store.LevelAct); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	fc.take()
	if writes.Load() != 1 {
		t.Fatalf("%d writes with a limit of 1 an hour", writes.Load())
	}
	a.Engine.HandleIncoming(ctx, dm("hal, check the light")) // reading isn't an action
	if reads.Load() != 3 {
		t.Fatalf("a read was held by the actions limit: %d", reads.Load())
	}

	// One integration's calls, from every sprig together.
	_ = a.Settings.Set(ctx, "limits.integration.ha.calls_per_minute", "3")
	a.Engine.HandleIncoming(ctx, dm("hal, check the light"))
	fc.take()
	if reads.Load() != 3 {
		t.Fatalf("the integration limit didn't hold: %d reads", reads.Load())
	}
	items, _ = a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 3 {
		t.Fatalf("want one inbox item per limit hit: %+v", items)
	}
}
