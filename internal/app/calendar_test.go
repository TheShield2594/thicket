package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

const sage = `---
id: sage
name: Sage
role: Keeps the household calendar.
personality:
  traits: [organised]
voice:
  style: [brief]
tools: ["calendar"]
---
Sage keeps track of things.
`

func ics(uid, title, start string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + title + "\r\nDTSTART:" + start + "\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func calendarServers(t *testing.T) (feed, caldav *httptest.Server) {
	feed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "secret-feed-token" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		io.WriteString(w, ics("dentist", "Owner's dentist", "20261120T140000Z"))
	}))
	caldav = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, _ := r.BasicAuth()
		if r.Method != "REPORT" || user != "kim" || pw != "dav-pass" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "time-range") {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(207)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/cal/1.ics</d:href>
<d:propstat><d:prop><c:calendar-data>`+ics("school", "School play", "20261121T180000Z")+`</c:calendar-data></d:prop></d:propstat></d:response></d:multistatus>`)
	}))
	t.Cleanup(feed.Close)
	t.Cleanup(caldav.Close)
	return
}

func titles(items []calendar.Item, layer string) string {
	var out []string
	for _, it := range items {
		if layer == "" || it.Layer == layer {
			out = append(out, it.Title)
		}
	}
	return strings.Join(out, " | ")
}

func TestHouseholdCalendar(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	a.Calendar.Now = func() time.Time { return now }
	feed, caldav := calendarServers(t)

	// The owner's private feed; Kim's CalDAV calendar, shared with the household.
	if _, err := a.Engine.AddCalendar(ctx, h.owner, "Mine", "ics", feed.URL+"/cal.ics?token=wrong", "", "", false); err == nil {
		t.Fatal("a calendar that doesn't load was saved")
	}
	if _, err := a.Engine.AddCalendar(ctx, h.owner, "Mine", "ics", feed.URL+"/cal.ics?token=secret-feed-token", "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.AddCalendar(ctx, h.kim, "Family", "caldav", caldav.URL+"/cal/", "kim", "dav-pass", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.AddCalendar(ctx, h.kid, "Mine", "ics", feed.URL, "", "", false); err == nil {
		t.Fatal("a kid connected a calendar")
	}
	srcs, _ := a.DB.CalendarSourcesFor(ctx, 0)
	for _, s := range srcs {
		if strings.Contains(s.URL, "secret-feed-token") || strings.Contains(s.URL, "http") {
			t.Fatalf("calendar address stored in the clear: %q", s.URL)
		}
	}

	// A sprig job: scheduled runs and one that failed.
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: h.owner.ID, Character: "hal", Name: "Lights check", Schedule: "0 9 * * *", Prompt: "x"})
	runID, _ := a.DB.StartJobRun(ctx, j.ID, now.Add(-time.Hour))
	a.DB.FinishJobRun(ctx, runID, "error", "", "boom", 0)

	// A family sprig puts dinner on the household calendar.
	c, _ := character.Parse([]byte(sage))
	if _, err := a.Chars.Write(ctx, c); err != nil {
		t.Fatal(err)
	}
	h.ownerSay("Sage, put dinner on the calendar")

	from, to := now.Add(-2*time.Hour), now.AddDate(0, 0, 4)
	view := func(u *store.User, f calendar.Filter) []calendar.Item {
		t.Helper()
		items, errs, err := a.Calendar.View(ctx, u, from, to, f)
		if err != nil || len(errs) > 0 {
			t.Fatalf("view: %v %v", err, errs)
		}
		return items
	}
	owner := view(h.owner, calendar.Filter{})
	if got := titles(owner, calendar.LayerEvents); got != "Owner's dentist | School play" {
		t.Fatalf("owner's events: %q", got)
	}
	if got := titles(owner, calendar.LayerOutputs); got != "Dinner: lentil curry" {
		t.Fatalf("owner's outputs: %q", got)
	}
	var failed, scheduled int
	for _, it := range owner {
		if it.Layer == calendar.LayerJobs && it.Sprig == "hal" {
			switch it.Status {
			case "failed":
				failed++
			case "scheduled":
				scheduled++
			}
		}
		if it.Layer == calendar.LayerOutputs && (it.SourceURL != "https://recipes.example/lentil-curry" || it.SourceLabel != "Recipe box" || !it.Shared) {
			t.Fatalf("output should link back to its source and be shared: %+v", it)
		}
	}
	if failed != 1 || scheduled != 4 {
		t.Fatalf("jobs layer: %d failed, %d scheduled", failed, scheduled)
	}

	// Kim sees the shared calendar and the household dinner, not the owner's own calendar or jobs.
	kim := view(h.kim, calendar.Filter{})
	if got := titles(kim, ""); got != "Dinner: lentil curry | School play" {
		t.Fatalf("Kim's calendar: %q", got)
	}

	// Filters: per layer and per sprig.
	if got := titles(view(h.owner, calendar.Filter{Layers: map[string]bool{calendar.LayerOutputs: true}}), ""); got != "Dinner: lentil curry" {
		t.Fatalf("outputs only: %q", got)
	}
	if got := titles(view(h.owner, calendar.Filter{Hide: map[string]bool{"sage": true}}), calendar.LayerOutputs); got != "" {
		t.Fatalf("Sage hidden: %q", got)
	}
	if got := titles(view(h.owner, calendar.Filter{Sprigs: map[string]bool{"sage": true}}), calendar.LayerJobs); got != "" {
		t.Fatalf("only Sage: Hal's jobs shown: %q", got)
	}

	// Kids: no calendar, unless a parent allows it, and then only shared things.
	if _, _, err := a.Calendar.View(ctx, h.kid, from, to, calendar.Filter{}); err == nil {
		t.Fatal("a kid read the household calendar")
	}
	if got := titles(view(h.kid, calendar.Filter{SharedOnly: true}), ""); got != "Dinner: lentil curry | School play" {
		t.Fatalf("shared only: %q", got)
	}

	// The sprig's entry is in the trail; only the person it was added for (or an admin) can undo it.
	trail, _ := a.DB.Trail(ctx, h.owner.ID, 5)
	var entry store.TrailEntry
	for _, e := range trail {
		if e.Kind == store.TrailEvent {
			entry = e
		}
	}
	if !entry.Undoable || !strings.Contains(entry.Summary, "lentil curry") {
		t.Fatalf("trail: %+v", trail)
	}
	if _, err := a.Engine.UndoTrail(ctx, h.kim, entry.ID); err == nil {
		t.Fatal("Kim undid the owner's entry")
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, entry.ID); err != nil {
		t.Fatal(err)
	}
	if got := titles(view(h.kim, calendar.Filter{}), calendar.LayerOutputs); got != "" {
		t.Fatalf("undone entry still shown: %q", got)
	}
}
