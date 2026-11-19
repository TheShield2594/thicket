package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

// clockAt gives the engine a clock the test moves (the owner is on UTC).
func clockAt(a *App, start time.Time) func(time.Time) {
	var mu sync.Mutex
	now := start
	a.Engine.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	return func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
}

func finishJob(ctx context.Context, a *App, j *store.Job, out string) {
	a.Engine.JobFinished(ctx, *j, scheduler.Result{Status: "ok", Output: out}, time.Time{})
}

// A job finishing at 02:00, inside Juni's quiet hours (22:00-08:00), is
// held, survives in the database, and is delivered after 08:00, once.
func TestJobOutputWaitsForQuietHoursToEnd(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	set := clockAt(a, day.Add(2*time.Hour))
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Night digest", Schedule: "0 2 * * *", Prompt: "x"})

	finishJob(ctx, a, j, "Overnight: tomatoes up 12%.")
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("job output sent at 02:00: %+v", out)
	}
	held, _ := a.DB.HeldMessages(ctx, owner.ID, "juniper")
	if len(held) != 1 || held[0].Reason != store.HeldQuiet || held[0].Ref != "job:"+itoa(j.ID) {
		t.Fatalf("held: %+v", held)
	}
	// The dashboard shows the budget and what's held.
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session, _ := a.DB.CreateWebSession(ctx, owner.ID, "test", web.SessionTTL)
	if _, body := getDashboard(t, srv, session); !strings.Contains(body, "Unprompted messages to you today: 0 of 3 · 1 held for later") {
		t.Fatalf("dashboard doesn't show the budget and held message:\n%s", body)
	}

	set(day.Add(7*time.Hour + 59*time.Minute))
	a.Engine.FlushHeld(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("delivered before quiet hours ended: %+v", out)
	}

	set(day.Add(8*time.Hour + 5*time.Minute))
	a.Engine.FlushHeld(ctx)
	out := fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "tomatoes up 12%") || !strings.Contains(out[0].text, "held until your quiet hours ended") {
		t.Fatalf("held job output after 08:00: %+v", out)
	}
	a.Engine.FlushHeld(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("held message delivered twice: %+v", out)
	}
	if held, _ := a.DB.HeldMessages(ctx, owner.ID, ""); len(held) != 0 {
		t.Fatalf("still held: %+v", held)
	}
}

// Three proactive messages a day per sprig and person by default: the
// fourth is held for the next day's budget, replies never count, and a
// message held more than a day goes to the inbox instead.
func TestFourthProactiveMessageIsHeld(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	set := clockAt(a, day.Add(10*time.Hour))
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Pings", Schedule: "0 * * * *", Prompt: "x"})

	for i := 1; i <= 3; i++ {
		finishJob(ctx, a, j, "ping "+itoa(int64(i)))
		a.Engine.HandleIncoming(ctx, dm("thanks!")) // a reply: doesn't count
	}
	if got := strings.Count(sentText(fc.take()), "ping "); got != 3 {
		t.Fatalf("first three proactive messages: %d sent", got)
	}
	finishJob(ctx, a, j, "ping 4")
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("fourth proactive message sent: %+v", out)
	}
	if held, _ := a.DB.HeldMessages(ctx, owner.ID, "juniper"); len(held) != 1 || held[0].Reason != store.HeldBudget {
		t.Fatalf("held: %+v", held)
	}
	// Later the same day it still waits.
	set(day.Add(20 * time.Hour))
	a.Engine.FlushHeld(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("delivered over budget: %+v", out)
	}

	// Next morning (after quiet hours) it goes out with the new budget.
	set(day.Add(24*time.Hour + 9*time.Hour))
	a.Engine.FlushHeld(ctx)
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "ping 4") || !strings.Contains(out[0].text, "used up the day's messages") {
		t.Fatalf("next day: %+v", out)
	}

	// Day two: 1 used by the held one, two more, then one held...
	finishJob(ctx, a, j, "ping 5")
	finishJob(ctx, a, j, "ping 6")
	finishJob(ctx, a, j, "ping 7")
	if got := strings.Count(sentText(fc.take()), "ping "); got != 2 {
		t.Fatalf("day two: %d sent", got)
	}
	// ...that's stale by the time the budget allows it: inbox, not chat.
	set(day.Add(48*time.Hour + 10*time.Hour))
	a.Engine.FlushHeld(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("stale held message sent: %+v", out)
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxUndelivered || !strings.Contains(items[0].Detail, "ping 7") {
		t.Fatalf("stale message not in the inbox: %+v", items)
	}
	acts, _ := a.DB.RecentActivity(ctx, 50, "warn")
	if !hasActivity(acts, "dropped held job") {
		t.Fatalf("drop not logged: %+v", acts)
	}
}

// An approval a job needs arrives at once, even at 02:00, and says why it
// got through; the job's own result waits for the morning.
func TestApprovalBypassesQuietHours(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	clockAt(a, day.Add(2*time.Hour))
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Pet notes", Schedule: "0 2 * * *", Prompt: "Remember my dog is called Biscuit."})

	res := a.Engine.RunJob(ctx, *j, 0)
	out := fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "I need your OK") || !strings.Contains(out[0].text, "during your quiet hours because it needs your approval") {
		t.Fatalf("approval at 02:00: %+v", out)
	}
	if _, err := a.DB.LatestPendingApproval(ctx, owner.ID, "discord:dm-owner", time.Hour); err != nil {
		t.Fatalf("approval not registered: %v", err)
	}
	a.Engine.JobFinished(ctx, *j, res, time.Time{})
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("job result sent at 02:00: %+v", out)
	}
}

// The budget and quiet hours can be changed in chat.
func TestSetInitiativeInChat(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("message me at most 5 a day"))
	juni, _ := a.Chars.Get("juniper")
	if juni.DailyBudget() != 5 || juni.Initiative.QuietHours != "21:30-07:00" {
		t.Fatalf("initiative: %+v", juni.Initiative)
	}
}
