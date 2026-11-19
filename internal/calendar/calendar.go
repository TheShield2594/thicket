package calendar

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
)

// Layers of the calendar view.
const (
	LayerEvents  = "events"  // calendars people connected
	LayerJobs    = "jobs"    // sprig jobs: scheduled, ran, failed
	LayerOutputs = "outputs" // things sprigs put on the calendar
)

// Item is one entry in the merged view.
type Item struct {
	Layer    string    `json:"layer"`
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	AllDay   bool      `json:"all_day,omitempty"`
	Location string    `json:"location,omitempty"`
	// Calendar names a connected calendar (events layer).
	Calendar string `json:"calendar,omitempty"`
	Shared   bool   `json:"shared,omitempty"`
	// Sprig is the sprig behind a job or output.
	Sprig string `json:"sprig,omitempty"`
	// Status of a job: scheduled, ran, failed (or running, skipped).
	Status string `json:"status,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// Details and the link back to where an output came from.
	Details     string `json:"details,omitempty"`
	SourceLabel string `json:"source_label,omitempty"`
	SourceURL   string `json:"source_url,omitempty"`
	// EventID names a sprig output; Removable says the person may take it
	// off the calendar (DELETE /api/v2/calendar/outputs/{event_id}).
	EventID   int64 `json:"event_id,omitempty"`
	Removable bool  `json:"removable,omitempty"`
}

// Filter picks what the view shows. Empty Layers means all of them;
// Sprigs, if set, keeps only those sprigs' jobs and outputs; Hide drops
// sprigs (per-sprig toggles off).
type Filter struct {
	Layers map[string]bool
	Sprigs map[string]bool
	Hide   map[string]bool
	// SharedOnly keeps only household (shared) calendars and outputs, and
	// no jobs: what a kid sprig sees when a parent allowed the calendar.
	SharedOnly bool
}

func (f Filter) layer(l string) bool { return len(f.Layers) == 0 || f.Layers[l] }
func (f Filter) sprig(id string) bool {
	return !f.Hide[id] && (len(f.Sprigs) == 0 || f.Sprigs[id])
}

// Secrets reads stored credentials ({{secret:NAME}} in a URL, and a
// source's password).
type Secrets interface {
	Get(ctx context.Context, name string) (string, error)
	Expand(ctx context.Context, s string) (string, error)
}

// Service builds the view.
type Service struct {
	DB      *store.Store
	Secrets Secrets
	HTTP    *http.Client
	// MCP reads calendars reached through an integration (kind mcp).
	MCP MCPCaller
	// CacheFor is how long a fetched calendar is reused (default 5 minutes).
	CacheFor time.Duration
	Now      func() time.Time

	mu    sync.Mutex
	cache map[int64]cached
}

type cached struct {
	at       time.Time
	from, to time.Time
	loc      string // the zone it was read in
	events   []Event
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// View is the calendar for one person: their own connected calendars and
// shared ones, their sprigs' jobs, and sprig outputs (theirs and shared).
// A calendar that fails to load is reported in errs, not as a failure.
func (s *Service) View(ctx context.Context, u *store.User, from, to time.Time, f Filter) (items []Item, errs []string, err error) {
	if u.IsKid() && !f.SharedOnly {
		return nil, nil, errors.New("kids don't have the household calendar")
	}
	if !to.After(from) || to.Sub(from) > 400*24*time.Hour {
		return nil, nil, errors.New("pick a range of up to 400 days")
	}
	loc := u.Location()
	if f.layer(LayerEvents) {
		srcs, err := s.DB.CalendarSourcesFor(ctx, u.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, src := range srcs {
			if !src.Enabled || f.SharedOnly && !src.Shared {
				continue
			}
			evs, err := s.events(ctx, src, from, to, loc)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", src.Name, err))
				_ = s.DB.SetCalendarSourceError(ctx, src.ID, err.Error())
				continue
			}
			if src.LastError != "" {
				_ = s.DB.SetCalendarSourceError(ctx, src.ID, "")
			}
			for _, e := range evs {
				items = append(items, Item{Layer: LayerEvents, ID: fmt.Sprintf("cal%d:%s:%d", src.ID, e.UID, e.Start.Unix()), Title: e.Title,
					Start: e.Start, End: e.End, AllDay: e.AllDay, Location: e.Location, Calendar: src.Name, Shared: src.Shared, SourceURL: e.URL})
			}
		}
	}
	if f.layer(LayerJobs) && !f.SharedOnly {
		jobs, err := s.jobs(ctx, u, from, to, loc, f)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, jobs...)
	}
	if f.layer(LayerOutputs) {
		outs, err := s.DB.SprigEventsFor(ctx, u.ID, from, to)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range outs {
			if !f.sprig(o.Character) || f.SharedOnly && !o.Shared {
				continue
			}
			items = append(items, Item{Layer: LayerOutputs, ID: fmt.Sprintf("out%d", o.ID), EventID: o.ID, Title: o.Title, Start: o.StartsAt, End: o.EndsAt,
				AllDay: o.AllDay, Sprig: o.Character, Kind: o.Kind, Details: o.Details, SourceLabel: o.SourceLabel, SourceURL: o.SourceURL, Shared: o.Shared,
				Removable: o.RemovableBy(u)})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
	return items, errs, nil
}

// jobs lists the person's sprig jobs in range: past runs with how they
// went, and upcoming scheduled runs.
func (s *Service) jobs(ctx context.Context, u *store.User, from, to time.Time, loc *time.Location, f Filter) ([]Item, error) {
	jobs, err := s.DB.UserJobs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	runs, err := s.DB.JobRunsBetween(ctx, u.ID, from, to)
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	var out []Item
	for _, r := range runs {
		j, ok := byID[r.JobID]
		if !ok || !f.sprig(j.Character) {
			continue
		}
		status := map[string]string{"ok": "ran", "empty": "ran", "error": "failed", "missed": "failed", "running": "running", "skipped": "skipped"}[r.Status]
		if status == "" {
			status = r.Status
		}
		out = append(out, Item{Layer: LayerJobs, ID: fmt.Sprintf("run%d", r.ID), Title: j.Name, Start: r.StartedAt, End: r.FinishedAt, Sprig: j.Character, Status: status})
	}
	now := s.now()
	for _, j := range jobs {
		if !j.Enabled || j.Schedule == "" || !f.sprig(j.Character) {
			continue
		}
		start := from
		if now.After(start) {
			start = now
		}
		for t := scheduler.Next(j.Schedule, start.Add(-time.Second), loc); !t.IsZero() && t.Before(to) && len(out) < 2000; t = scheduler.Next(j.Schedule, t, loc) {
			out = append(out, Item{Layer: LayerJobs, ID: fmt.Sprintf("job%d:%d", j.ID, t.Unix()), Title: j.Name, Start: t, Sprig: j.Character, Status: "scheduled"})
		}
	}
	return out, nil
}

func (s *Service) events(ctx context.Context, src store.CalendarSource, from, to time.Time, loc *time.Location) ([]Event, error) {
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[int64]cached{}
	}
	ttl := s.CacheFor
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	// Floating and local times are read in the viewer's zone, so a cached
	// copy is only for the same zone.
	if c, ok := s.cache[src.ID]; ok && c.loc == loc.String() && s.now().Sub(c.at) < ttl && !from.Before(c.from) && !to.After(c.to) {
		s.mu.Unlock()
		return filterRange(c.events, from, to), nil
	}
	s.mu.Unlock()
	evs, err := s.load(ctx, src, from, to, loc)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[src.ID] = cached{at: s.now(), from: from, to: to, loc: loc.String(), events: evs}
	s.mu.Unlock()
	return evs, nil
}

// load reads a source's events in range, whatever its kind.
func (s *Service) load(ctx context.Context, src store.CalendarSource, from, to time.Time, loc *time.Location) ([]Event, error) {
	if src.Kind == "mcp" {
		m, err := ParseMCPSource(src.Config)
		if err != nil {
			return nil, err
		}
		return s.fetchMCP(ctx, m, from, to, loc)
	}
	data, err := s.fetch(ctx, src, from, to)
	if err != nil {
		return nil, err
	}
	return ParseICS(data, from, to, loc)
}

// Forget drops a calendar from the cache (after it changes or is removed).
func (s *Service) Forget(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, id)
}

func filterRange(evs []Event, from, to time.Time) []Event {
	var out []Event
	for _, e := range evs {
		if e.Start.Before(to) && (e.End.After(from) || !e.Start.Before(from)) {
			out = append(out, e)
		}
	}
	return out
}

const maxCalendarBytes = 8 << 20

// fetch gets a source's iCalendar data: the feed itself, or, for CalDAV,
// the events in range through a calendar-query REPORT.
func (s *Service) fetch(ctx context.Context, src store.CalendarSource, from, to time.Time) (string, error) {
	raw, err := s.Secrets.Expand(ctx, src.URL)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(raw, "webcal://") {
		raw = "https://" + strings.TrimPrefix(raw, "webcal://")
	}
	if err := CheckURL(raw); err != nil {
		return "", err
	}
	var req *http.Request
	if src.Kind == "caldav" {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop><c:calendar-data/></d:prop>
  <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
    <c:time-range start="%s" end="%s"/>
  </c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`, from.UTC().Format("20060102T150405Z"), to.UTC().Format("20060102T150405Z"))
		req, err = http.NewRequestWithContext(ctx, "REPORT", raw, bytes.NewBufferString(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		req.Header.Set("Depth", "1")
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "text/calendar")
	}
	if src.Username != "" || src.Secret != "" {
		pw := ""
		if src.Secret != "" {
			if pw, err = s.Secrets.Get(ctx, src.Secret); err != nil {
				return "", fmt.Errorf("its password isn't stored")
			}
		}
		req.SetBasicAuth(src.Username, pw)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach it: %v", redactURL(err, raw))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxCalendarBytes+1))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("it refused the username or password (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if len(b) > maxCalendarBytes {
		return "", errors.New("the calendar is larger than 8 MB")
	}
	if src.Kind != "caldav" {
		return string(b), nil
	}
	return calendarData(b)
}

// calendarData joins the calendar-data of a CalDAV multistatus response.
func calendarData(b []byte) (string, error) {
	var ms struct {
		Responses []struct {
			Propstat []struct {
				Data string `xml:"prop>calendar-data"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(b, &ms); err != nil {
		return "", fmt.Errorf("unreadable CalDAV response: %v", err)
	}
	var out strings.Builder
	for _, r := range ms.Responses {
		for _, p := range r.Propstat {
			out.WriteString(p.Data)
			out.WriteString("\n")
		}
	}
	return out.String(), nil
}

// CheckURL accepts http(s) calendar addresses.
func CheckURL(raw string) error {
	if strings.HasPrefix(raw, "webcal://") {
		raw = "https://" + strings.TrimPrefix(raw, "webcal://")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the calendar address must be an http(s) or webcal link")
	}
	return nil
}

// redactURL keeps a private feed address (which often holds a token) out
// of error messages.
func redactURL(err error, raw string) string {
	msg := err.Error()
	if raw != "" {
		msg = strings.ReplaceAll(msg, raw, "the calendar address")
	}
	return msg
}

// Probe loads a calendar once, without caching: to check it works before
// it's saved.
func (s *Service) Probe(ctx context.Context, src store.CalendarSource, from, to time.Time, loc *time.Location) ([]Event, string, error) {
	if src.Kind == "mcp" {
		evs, err := s.load(ctx, src, from, to, loc)
		return evs, "", err
	}
	data, err := s.fetch(ctx, src, from, to)
	if err != nil {
		return nil, "", err
	}
	evs, err := ParseICS(data, from, to, loc)
	if err != nil {
		return nil, "", err
	}
	if src.Kind == "ics" && !strings.Contains(data, "BEGIN:VCALENDAR") {
		return nil, "", errors.New("that address doesn't return a calendar (no BEGIN:VCALENDAR)")
	}
	return evs, data, nil
}
