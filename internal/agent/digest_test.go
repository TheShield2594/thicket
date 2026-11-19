package agent

import (
	"testing"
	"time"
)

func TestLastDigestTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, ny)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ now, want string }{
		{"2026-11-15 18:00", "2026-11-15 18:00"}, // Sunday, on the minute
		{"2026-11-15 17:59", "2026-11-08 18:00"}, // Sunday, just before
		{"2026-11-18 09:00", "2026-11-15 18:00"}, // Wednesday
		{"2026-11-07 12:00", "2026-11-01 18:00"}, // across the end of daylight saving (1 Nov)
	} {
		got := lastDigestTime(at(c.now), ny, time.Sunday, 18*60)
		if !got.Equal(at(c.want)) {
			t.Errorf("at %s: got %s, want %s", c.now, got.In(ny), c.want)
		}
	}
}

func TestDigestTextQuietWeek(t *testing.T) {
	from := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)
	d := &Digest{From: from, To: from.AddDate(0, 0, 7), Sprigs: []DigestSprig{{ID: "juniper", Name: "Juni", CostUSD: 0.001}}}
	got := d.Text("http://thicket.local/dashboard")
	want := "**Your week** (Sun 8 Nov – Sun 15 Nov): a quiet one. Nothing ran, nothing changed and nothing is waiting on you.\nMore on the dashboard: http://thicket.local/dashboard"
	if got != want {
		t.Fatalf("got %q", got)
	}
}
