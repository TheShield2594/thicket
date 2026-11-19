package app

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// etcZone names the fixed timezone off hours ahead of UTC (the Etc names'
// signs are inverted).
func etcZone(off int) string {
	switch {
	case off > 0:
		return fmt.Sprintf("Etc/GMT-%d", off)
	case off < 0:
		return fmt.Sprintf("Etc/GMT+%d", -off)
	}
	return "Etc/GMT"
}

// A household with two adults and a kid: each adult gets their own weekly
// digest at the set time in their own timezone, with their solo sprigs,
// the family sprigs and (for the parent) the kid sprig, the right
// totals, and nothing of the other adult's private sprig. The kid gets
// none. It goes out once a week, outside the daily budget, and a quiet
// week sends one line, or nothing for someone who asked for that.
func TestWeeklyDigest(t *testing.T) {
	h := setupHousehold(t)
	a, ctx, fc := h.a, h.ctx, h.fc
	writeSolo(t, a, "ledger", h.owner.ID) // the owner's private sprig
	writeSolo(t, a, "wren", h.kim.ID)     // Kim's

	// The kid chats with Bramble (the owner is Bramble's parent).
	h.kidSays("hi!")
	h.kidSays("tell me a riddle")
	fc.take()

	// Timezones: the afternoon for the owner an hour from now, and three
	// hours behind for Kim (both outside Juni's quiet hours, 22:00-08:00).
	now := time.Now()
	ownerOff := 12
	for ; ownerOff > -9; ownerOff-- {
		if h := now.Add(time.Hour).UTC().Add(time.Duration(ownerOff) * time.Hour).Hour(); h >= 11 && h <= 19 {
			break
		}
	}
	if err := a.DB.UpdateUserProfile(ctx, h.owner.ID, "", etcZone(ownerOff)); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.UpdateUserProfile(ctx, h.kim.ID, "", etcZone(ownerOff-3)); err != nil {
		t.Fatal(err)
	}
	owner, _ := a.DB.UserByID(ctx, h.owner.ID)

	// Spend this week, exactly: only what's recorded here.
	if _, err := a.DB.DB().ExecContext(ctx, "DELETE FROM usage"); err != nil {
		t.Fatal(err)
	}
	for sprig, usd := range map[string]float64{"juniper": 0.10, "hal": 0.20, "ledger": 0.40, "wren": 0.80, "bramble": 0.05} {
		if err := a.DB.RecordUsage(ctx, store.Usage{Character: sprig, Purpose: "chat", Model: "fake", CostUSD: usd}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Settings.Set(ctx, "budget.character.hal.daily_usd", "1"); err != nil {
		t.Fatal(err)
	}

	// Jobs: two runs of the owner's (one failed), one of Kim's with Hal.
	run := func(userID int64, sprig, name, status string, n int) {
		j, err := a.DB.CreateJob(ctx, store.Job{UserID: userID, Character: sprig, Name: name, Schedule: "0 9 * * *", Prompt: "x"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			id, _ := a.DB.StartJobRun(ctx, j.ID, time.Now())
			st := status
			if i == 1 {
				st = "error"
			}
			_ = a.DB.FinishJobRun(ctx, id, st, "", "", 0)
		}
	}
	run(owner.ID, "juniper", "Morning news", "ok", 2)
	run(h.kim.ID, "hal", "Lights check", "ok", 1)
	run(h.kim.ID, "wren", "WRENSECRET reading", "ok", 1)

	// What's waiting, and what changed.
	add := func(it store.InboxItem) {
		if _, err := a.DB.AddInbox(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	add(store.InboxItem{UserID: owner.ID, Character: "ledger", Kind: store.InboxDraft, Title: "Ledger drafted: pay the LEDGERSECRET bill"})
	add(store.InboxItem{UserID: h.kim.ID, Character: "wren", Kind: store.InboxFailed, Ref: "job:99", Title: "Job “WRENSECRET list” failed"})
	add(store.InboxItem{UserID: h.kim.ID, Character: "hal", Kind: store.InboxFailed, Ref: "job:98", Title: "Job “Lights check” failed"})
	if _, err := a.DB.CreateApproval(ctx, store.Approval{UserID: owner.ID, Character: "juniper", ChatRef: "discord:dm-owner", Tool: "send_email", Args: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []store.TrailEntry{
		{UserID: h.kim.ID, Character: "wren", Kind: store.TrailEvent, Summary: "put WRENSECRET on the calendar"},
		{UserID: owner.ID, Character: "ledger", Kind: store.TrailEvent, Summary: "put LEDGERSECRET on the calendar"},
		{UserID: owner.ID, Character: "hal", Kind: store.TrailEvent, Summary: "put Bins night on the calendar"},
		{UserID: h.kid.ID, Character: "bramble", Kind: store.TrailSafety, Summary: "safety alert (bullying) for Max"},
	} {
		if err := a.DB.AddTrail(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	// The digest is due ten minutes before the engine's clock, in the
	// owner's timezone; for Kim, three hours later.
	var mu sync.Mutex
	clock := now.Add(time.Hour)
	a.Engine.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	setClock := func(t time.Time) { mu.Lock(); clock = t; mu.Unlock() }
	due := clock.Add(-10 * time.Minute).In(owner.Location())
	if err := a.Settings.Set(ctx, "digest.day", strings.ToLower(due.Weekday().String())); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "digest.time", due.Format("15:04")); err != nil {
		t.Fatal(err)
	}
	// Use up the host's daily budget for both: the digest is exempt.
	for i := 0; i < 10; i++ {
		_ = a.DB.RecordProactive(ctx, owner.ID, "juniper", clock)
		_ = a.DB.RecordProactive(ctx, h.kim.ID, "juniper", clock.Add(3*time.Hour))
	}

	to := func(out []sent, chat string) []string {
		var got []string
		for _, s := range out {
			if s.chat == chat {
				got = append(got, s.text)
			}
		}
		return got
	}
	mustOne := func(out []sent, chat string) string {
		t.Helper()
		got := to(out, chat)
		if len(got) != 1 {
			t.Fatalf("%s got %d messages: %q (all: %+v)", chat, len(got), got, out)
		}
		return got[0]
	}

	a.Engine.WeeklyDigests(ctx)
	out := fc.take()
	mine := mustOne(out, "dm-owner")
	t.Logf("the owner's digest:\n%s", mine)
	if len(to(out, "dm-kim")) != 0 || len(to(out, "dm-max")) != 0 {
		t.Fatalf("only the owner's digest is due yet: %+v", out)
	}
	for _, s := range out {
		if s.persona != "Juni" {
			t.Fatalf("digest not from the host: %+v", s)
		}
	}
	for _, want := range []string{
		"**Juni** · $0.10, no cap of its own", "2 job runs (1 failed)", "Needs you: approve send_email",
		"**Hal** · $0.20 of $7.00 cap ($1.00 a day)", "Changed: put Bins night on the calendar",
		"**Ledger** (yours) · $0.40", "Needs you: Ledger drafted: pay the LEDGERSECRET bill", "Changed: put LEDGERSECRET on the calendar",
		"**Bramble** (for Max) · $0.05", "replies to Max", "1 safety alert",
		"Total: $0.75 for these sprigs this week.", "/dashboard",
	} {
		if !strings.Contains(mine, want) {
			t.Errorf("owner's digest lacks %q:\n%s", want, mine)
		}
	}
	for _, never := range []string{"Wren", "WRENSECRET", "$0.80", "Lights check"} {
		if strings.Contains(mine, never) {
			t.Errorf("owner's digest shows %q:\n%s", never, mine)
		}
	}

	// Once a week: running again sends nothing more.
	a.Engine.WeeklyDigests(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("sent again: %+v", out)
	}

	// Three hours later it's Kim's time.
	setClock(clock.Add(3 * time.Hour))
	a.Engine.WeeklyDigests(ctx)
	out = fc.take()
	hers := mustOne(out, "dm-kim")
	if len(to(out, "dm-owner")) != 0 || len(to(out, "dm-max")) != 0 {
		t.Fatalf("only Kim's digest is due: %+v", out)
	}
	for _, want := range []string{
		"**Juni** · $0.10", "**Hal** · $0.20 of $7.00 cap", "1 job run", "Needs you: Job “Lights check” failed",
		"**Wren** (yours) · $0.80", "WRENSECRET", "Total: $1.10 for these sprigs this week.",
	} {
		if !strings.Contains(hers, want) {
			t.Errorf("Kim's digest lacks %q:\n%s", want, hers)
		}
	}
	// Not the owner's private sprig, not the kid sprig (she isn't its
	// parent), not the owner's own items, nor the owner's change to a
	// family sprig (she isn't an admin).
	for _, never := range []string{"Ledger", "LEDGERSECRET", "$0.40", "Bramble", "Max", "send_email", "Bins night", "Morning news"} {
		if strings.Contains(hers, never) {
			t.Errorf("Kim's digest shows %q:\n%s", never, hers)
		}
	}

	// A week later, with nothing new: the owner gets one line, Kim (who
	// asked for nothing in a quiet week) gets nothing.
	for _, u := range []int64{owner.ID, h.kim.ID} {
		items, _ := a.DB.OpenInbox(ctx, u)
		for _, it := range items {
			_ = a.DB.ResolveInbox(ctx, it.ID, 0)
		}
	}
	if _, err := a.DB.DB().ExecContext(ctx, "UPDATE approvals SET status = 'denied'"); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.SetDigestMode(ctx, h.kim.ID, store.DigestSkipEmpty); err != nil {
		t.Fatal(err)
	}
	setClock(now.Add(7*24*time.Hour + time.Hour))
	a.Engine.WeeklyDigests(ctx)
	out = fc.take()
	if line := mustOne(out, "dm-owner"); !strings.Contains(line, "a quiet one") || strings.Count(line, "\n") > 1 {
		t.Fatalf("quiet week: %q", line)
	}
	setClock(now.Add(7*24*time.Hour + 4*time.Hour))
	a.Engine.WeeklyDigests(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("Kim asked for nothing in a quiet week: %+v", out)
	}

	// Off for the household: nothing at all.
	if err := a.Settings.Set(ctx, "digest.day", "off"); err != nil {
		t.Fatal(err)
	}
	setClock(now.Add(14*24*time.Hour + time.Hour))
	a.Engine.WeeklyDigests(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("digest off, but sent: %+v", out)
	}
}
