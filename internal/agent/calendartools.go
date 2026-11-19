package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Calendar tools, for sprigs with the calendar scope. Reading shows the
// person's calendar (their own connected calendars, shared household ones,
// their sprigs' jobs and outputs). Writing puts a "sprig output" on it:
// recorded in the trail and undoable.

func (e *Engine) calendarTools() []*Tool {
	return []*Tool{
		{
			Name: "calendar",
			Description: "Read the calendar: the user's connected calendars and shared household ones (events), sprigs' scheduled jobs and how they ran (jobs), " +
				"and things sprigs put on it (outputs). Defaults to the next 7 days, all layers.",
			Params: `{"type":"object","properties":{"from":{"type":"string","description":"YYYY-MM-DD (default today)"},"days":{"type":"integer","description":"how many days (default 7, at most 60)"},` +
				`"layers":{"type":"array","items":{"type":"string","enum":["events","jobs","outputs"]}}}}`,
			Scope:     character.ScopeCalendar,
			Untrusted: true,
			Run:       e.toolCalendar,
		},
		{
			Name: "add_to_calendar",
			Description: "Put something on the calendar as a sprig output: a dinner from a meal plan, a release date, a reminder. Link it to where it came from (source_label and source_url). " +
				"start is local time: YYYY-MM-DD HH:MM, or YYYY-MM-DD for all day. Family sprigs add to the shared household calendar by default; others to the user's own. It can be undone from the trail.",
			Params: `{"type":"object","properties":{"title":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"kind":{"type":"string","description":"e.g. dinner, release, reminder"},` +
				`"details":{"type":"string"},"source_label":{"type":"string","description":"where it came from, e.g. Mealie"},"source_url":{"type":"string"},"shared":{"type":"boolean"}},"required":["title","start"]}`,
			Scope:   character.ScopeCalendar,
			Writes:  true,
			Trailed: true,
			Run:     e.toolAddToCalendar,
		},
		{
			Name:        "remove_from_calendar",
			Description: "Take something a sprig put on the calendar off it again, by its event_id (from the calendar tool).",
			Params:      `{"type":"object","properties":{"event_id":{"type":"integer"}},"required":["event_id"]}`,
			Scope:       character.ScopeCalendar,
			Writes:      true,
			Trailed:     true,
			Run:         e.toolRemoveFromCalendar,
		},
	}
}

func (e *Engine) toolCalendar(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	if e.Calendar == nil {
		return "", errors.New("the calendar isn't available")
	}
	a, err := decode[struct {
		From   string   `json:"from"`
		Days   int      `json:"days"`
		Layers []string `json:"layers"`
	}](args)
	if err != nil {
		return "", err
	}
	loc := t.user.Location()
	now := e.now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if a.From != "" {
		if from, err = time.ParseInLocation("2006-01-02", a.From, loc); err != nil {
			return "", errors.New("from must look like 2026-11-20")
		}
	}
	days := a.Days
	if days <= 0 {
		days = 7
	}
	days = min(days, 60)
	f := calendar.Filter{Layers: map[string]bool{}}
	for _, l := range a.Layers {
		f.Layers[l] = true
	}
	if t.char.IsKid() {
		f.SharedOnly = true // a parent allowed the household calendar, nothing more
	}
	items, errs, err := e.Calendar.View(ctx, t.user, from, from.AddDate(0, 0, days), f)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	day := ""
	for _, it := range items {
		start := it.Start.In(loc)
		if d := start.Format("Mon 2 Jan"); d != day {
			day = d
			fmt.Fprintf(&b, "\n%s\n", d)
		}
		when := start.Format("15:04")
		if it.AllDay {
			when = "all day"
		}
		line := fmt.Sprintf("- %s %s", when, it.Title)
		switch it.Layer {
		case calendar.LayerEvents:
			line += " (" + it.Calendar + ")"
			if it.Location != "" {
				line += " at " + it.Location
			}
		case calendar.LayerJobs:
			line += fmt.Sprintf(" [job by %s: %s]", it.Sprig, it.Status)
		case calendar.LayerOutputs:
			line += fmt.Sprintf(" [added by %s, event_id %d]", it.Sprig, it.EventID)
			if it.SourceLabel != "" {
				line += " from " + it.SourceLabel
			}
		}
		b.WriteString(line + "\n")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = fmt.Sprintf("Nothing on the calendar for %d days from %s.", days, from.Format("Mon 2 Jan"))
	}
	if len(errs) > 0 {
		out += "\n(Couldn't load: " + strings.Join(errs, "; ") + ")"
	}
	return out, nil
}

// parseLocal reads "2006-01-02 15:04" (or with a T), "2006-01-02", or
// RFC 3339.
func parseLocal(s string, loc *time.Location) (t time.Time, allDay bool, err error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, false, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%q isn't a date: use YYYY-MM-DD HH:MM or YYYY-MM-DD", s)
}

func (e *Engine) toolAddToCalendar(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Title       string `json:"title"`
		Start       string `json:"start"`
		End         string `json:"end"`
		Kind        string `json:"kind"`
		Details     string `json:"details"`
		SourceLabel string `json:"source_label"`
		SourceURL   string `json:"source_url"`
		Shared      *bool  `json:"shared"`
	}](args)
	if err != nil {
		return "", err
	}
	if t.user.IsKid() || t.char.IsKid() {
		return "", errors.New("kid sprigs can't change the calendar")
	}
	title := strings.TrimSpace(a.Title)
	if title == "" || len(title) > 200 {
		return "", errors.New("title is required (at most 200 characters)")
	}
	loc := t.user.Location()
	start, allDay, err := parseLocal(a.Start, loc)
	if err != nil {
		return "", err
	}
	var end time.Time
	if a.End != "" {
		if end, _, err = parseLocal(a.End, loc); err != nil {
			return "", err
		}
		if !end.After(start) {
			return "", errors.New("end must be after start")
		}
	}
	if a.SourceURL != "" {
		if u, err := url.Parse(a.SourceURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return "", errors.New("source_url must be an http(s) link")
		}
	}
	family := t.char.Tier() == character.TierFamily
	shared := family
	if a.Shared != nil {
		shared = *a.Shared
	}
	if shared && !family {
		return "", errors.New("only family sprigs add to the shared household calendar")
	}
	kind := strings.TrimSpace(a.Kind)
	if kind == "" {
		kind = "event"
	}
	ev := store.SprigEvent{UserID: t.user.ID, Character: t.char.ID, Kind: truncate(kind, 30), Title: title, StartsAt: start, EndsAt: end, AllDay: allDay,
		Details: truncate(a.Details, 4000), SourceLabel: truncate(a.SourceLabel, 100), SourceURL: a.SourceURL, Shared: shared}
	id, err := e.DB.AddSprigEvent(ctx, ev)
	if err != nil {
		return "", err
	}
	where := "the user's calendar"
	if shared {
		where = "the household calendar"
	}
	when := start.Format("Mon 2 Jan 15:04")
	if allDay {
		when = start.Format("Mon 2 Jan")
	}
	trailID := e.addTrail(ctx, t.user.ID, t.char.ID, store.TrailEvent, fmt.Sprintf("%s added %q to %s (%s)", t.char.DisplayName(), title, where, when),
		strings.TrimSpace(a.SourceLabel+" "+a.SourceURL), &trailUndo{Type: "event", EventID: id})
	return fmt.Sprintf("Added %q on %s to %s (event_id %d, trail #%d: it can be undone).", title, when, where, id, trailID), nil
}

func (e *Engine) toolRemoveFromCalendar(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		EventID int64 `json:"event_id"`
	}](args)
	if err != nil {
		return "", err
	}
	if t.user.IsKid() || t.char.IsKid() {
		return "", errors.New("kid sprigs can't change the calendar")
	}
	ev, err := e.DB.SprigEventByID(ctx, a.EventID)
	if err != nil || (ev.UserID != t.user.ID && !(ev.Shared && t.char.Tier() == character.TierFamily)) {
		return "", fmt.Errorf("no event %d on this calendar", a.EventID)
	}
	if _, err := e.takeOffCalendar(ctx, t.user.ID, t.char.ID, t.char.DisplayName(), "", ev); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed %q.", ev.Title), nil
}

// RemoveSprigEvent takes a sprig output off the calendar for a person (the
// web app's Calendar screen and API v2): their own, or a shared one when
// they're an adult. Like remove_from_calendar it goes in the trail, so it
// can be put back. It returns the trail entry that undoes it.
func (e *Engine) RemoveSprigEvent(ctx context.Context, by *store.User, id int64) (*store.SprigEvent, int64, error) {
	if by.IsKid() {
		return nil, 0, errors.New("kids can't change the calendar")
	}
	ev, err := e.DB.SprigEventByID(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, 0, err
	}
	if err != nil || !ev.RemovableBy(by) {
		return nil, 0, fmt.Errorf("no event %d on your calendar: %w", id, store.ErrNotFound)
	}
	added := ev.Character
	if c, ok := e.Chars.Get(ev.Character); ok {
		added = c.DisplayName()
	}
	who := by.Name
	if who == "" {
		who = "someone"
	}
	trailID, err := e.takeOffCalendar(ctx, by.ID, ev.Character, who, "added by "+added, ev)
	if err != nil {
		return nil, 0, err
	}
	return ev, trailID, nil
}

// takeOffCalendar removes a sprig output and records it in the trail with
// what puts it back. who names whoever removed it (a sprig or a person).
func (e *Engine) takeOffCalendar(ctx context.Context, userID int64, char, who, detail string, ev *store.SprigEvent) (int64, error) {
	undo, err := json.Marshal(trailUndo{Type: "restore_event", EventID: ev.ID})
	if err != nil {
		return 0, err
	}
	return e.DB.DeleteSprigEventWithTrail(ctx, ev.ID, store.TrailEntry{UserID: userID, Character: char, Kind: store.TrailEvent,
		Summary: fmt.Sprintf("%s removed %q from the calendar", who, ev.Title), Detail: detail, Undo: undo})
}

// AddCalendar connects one of an adult's calendars, read-only, after
// checking it loads. The address (ICS links usually hold a private token)
// and the password go in the encrypted store, not the database.
func (e *Engine) AddCalendar(ctx context.Context, u *store.User, name, kind, rawURL, username, password string, shared bool) (*store.CalendarSource, error) {
	if u.IsKid() {
		return nil, errors.New("kids can't connect calendars")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return nil, errors.New("name is required (at most 60 characters)")
	}
	if kind != "ics" && kind != "caldav" {
		return nil, errors.New("kind must be ics (a calendar feed link) or caldav")
	}
	if err := calendar.CheckURL(rawURL); err != nil {
		return nil, err
	}
	suffix := strings.ToLower(randomToken(6))
	urlSecret := "calendar_" + suffix + "_url"
	if err := e.Secrets.Put(ctx, urlSecret, rawURL); err != nil {
		return nil, err
	}
	src := store.CalendarSource{UserID: u.ID, Name: name, Kind: kind, URL: "{{secret:" + urlSecret + "}}", Username: username, Shared: shared}
	if password != "" {
		src.Secret = "calendar_" + suffix + "_pw"
		if err := e.Secrets.Put(ctx, src.Secret, password); err != nil {
			return nil, err
		}
	}
	cleanup := func() {
		_ = e.Secrets.Delete(ctx, urlSecret)
		if src.Secret != "" {
			_ = e.Secrets.Delete(ctx, src.Secret)
		}
	}
	now := e.now()
	if _, _, err := e.Calendar.Probe(ctx, src, now.AddDate(0, 0, -1), now.AddDate(0, 0, 14), u.Location()); err != nil {
		cleanup()
		return nil, fmt.Errorf("couldn't read that calendar: %v", err)
	}
	id, err := e.DB.AddCalendarSource(ctx, src)
	if err != nil {
		cleanup()
		return nil, err
	}
	src.ID = id
	who := "their own"
	if shared {
		who = "shared with the household"
	}
	e.logActivity(ctx, "info", "calendar", "", u.ID, fmt.Sprintf("calendar %q connected (%s, %s)", name, kind, who), "")
	return &src, nil
}

// RemoveCalendar disconnects one of a person's calendars and forgets its
// address and password.
func (e *Engine) RemoveCalendar(ctx context.Context, u *store.User, id int64) error {
	srcs, err := e.DB.CalendarSourcesFor(ctx, u.ID)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		if s.ID != id || s.UserID != u.ID {
			continue
		}
		if err := e.DB.DeleteCalendarSource(ctx, u.ID, id); err != nil {
			return err
		}
		if name, ok := strings.CutPrefix(s.URL, "{{secret:"); ok {
			_ = e.Secrets.Delete(ctx, strings.TrimSuffix(name, "}}"))
		}
		if s.Secret != "" {
			_ = e.Secrets.Delete(ctx, s.Secret)
		}
		e.Calendar.Forget(id)
		e.logActivity(ctx, "info", "calendar", "", u.ID, fmt.Sprintf("calendar %q disconnected", s.Name), "")
		return nil
	}
	return store.ErrNotFound
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
