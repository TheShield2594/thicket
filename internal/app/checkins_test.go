package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

const sunny = `---
id: sunny
name: Sunny
role: Keeps the day on track.
personality:
  traits: [bright]
voice:
  style: [short and warm]
initiative:
  check_ins: false
  follow_ups: false
  morning_brief: "07:30"
  event_reminders: 30
  due_reminders: true
  daily_messages: 10
---
Sunny likes mornings.
`

// A sprig with the presets on sends the morning brief at the set time
// (today's calendar and anything due), one heads-up before an event, and
// one reminder before something due: each once, on a fake clock.
func TestCheckInPresets(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "sunny.md"), []byte(sunny), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	set := clockAt(a, day.Add(7*time.Hour))
	if _, err := a.DB.AddSprigEvent(ctx, store.SprigEvent{UserID: owner.ID, Character: "juniper", Kind: "event", Title: "Dentist",
		StartsAt: day.Add(15 * time.Hour), EndsAt: day.Add(16 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add", Memory: store.Memory{UserID: owner.ID, Kind: "followup",
		Content: "Pay the water bill.", DueAt: day.Add(17 * time.Hour), Source: "auto"}}}); err != nil {
		t.Fatal(err)
	}
	fromSunny := func() []string {
		var out []string
		for _, s := range fc.take() {
			if s.persona == "Sunny" {
				out = append(out, s.text)
			}
		}
		return out
	}

	// Before the set time: nothing.
	a.Engine.Presets(ctx)
	if out := fromSunny(); len(out) != 0 {
		t.Fatalf("brief before 07:30: %q", out)
	}
	// At the set time: one brief with today's calendar and what's due.
	set(day.Add(7*time.Hour + 35*time.Minute))
	a.Engine.Presets(ctx)
	out := fromSunny()
	if len(out) != 1 || !strings.Contains(out[0], "[Morning brief]") || !strings.Contains(out[0], "15:00 Dentist") || !strings.Contains(out[0], "Pay the water bill. (due 17:00)") {
		t.Fatalf("morning brief: %q", out)
	}
	set(day.Add(7*time.Hour + 50*time.Minute))
	a.Engine.Presets(ctx)
	if out := fromSunny(); len(out) != 0 {
		t.Fatalf("second brief: %q", out)
	}

	// The event: nothing until 30 minutes before, then one heads-up.
	set(day.Add(14*time.Hour + 20*time.Minute))
	a.Engine.Presets(ctx)
	if out := fromSunny(); len(out) != 1 || !strings.Contains(out[0], "[Due reminder]") || !strings.Contains(out[0], "Pay the water bill") {
		t.Fatalf("at 14:20 want only the due reminder (17:00 is within 3 hours): %q", out)
	}
	set(day.Add(14*time.Hour + 35*time.Minute))
	a.Engine.Presets(ctx)
	out = fromSunny()
	if len(out) != 1 || !strings.Contains(out[0], "[Event heads-up]") || !strings.Contains(out[0], `"Dentist" at 15:00 today (in 25 minutes`) {
		t.Fatalf("event heads-up: %q", out)
	}
	for _, at := range []time.Duration{14*time.Hour + 45*time.Minute, 15*time.Hour + 5*time.Minute, 16 * time.Hour} {
		set(day.Add(at))
		a.Engine.Presets(ctx)
		if out := fromSunny(); len(out) != 0 {
			t.Fatalf("again at %v: %q", at, out)
		}
	}
	// They went the proactive way: counted in Sunny's daily budget.
	if n, _ := a.DB.ProactiveSentSince(ctx, owner.ID, "sunny", day); n != 3 {
		t.Fatalf("proactive count: %d", n)
	}
}

// A brief whose time falls in quiet hours waits for them to end.
func TestMorningBriefWaitsForQuietHours(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	quiet := strings.Replace(sunny, "  daily_messages: 10", "  daily_messages: 10\n  quiet_hours: \"22:00-08:00\"", 1)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "sunny.md"), []byte(quiet), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	set := clockAt(a, day.Add(7*time.Hour+35*time.Minute))
	a.Engine.Presets(ctx)
	if out := sentText(fc.take()); strings.Contains(out, "Morning brief") {
		t.Fatalf("brief sent in quiet hours: %s", out)
	}
	set(day.Add(8*time.Hour + 5*time.Minute))
	a.Engine.Presets(ctx)
	if out := sentText(fc.take()); !strings.Contains(out, "Morning brief") {
		t.Fatalf("brief not sent after quiet hours: %s", out)
	}
}

// Kid sprigs never send briefs or reminders.
func TestKidSprigHasNoPresets(t *testing.T) {
	c := &character.Character{ID: "moss", Name: "Moss", Role: "A buddy.", Household: character.TierKid, Kids: []int64{2}, Parents: []int64{1},
		Personality: character.Personality{Traits: []string{"kind"}}, Voice: character.Voice{Style: []string{"short"}},
		Initiative: character.Initiative{MorningBrief: "07:30"}}
	if probs := strings.Join(c.Validate(), "; "); !strings.Contains(probs, "a kid sprig doesn't check in") {
		t.Fatalf("kid sprig with a morning brief passed validation: %q", probs)
	}
}
