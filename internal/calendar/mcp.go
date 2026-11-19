package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// A calendar reached through an integration (an MCP server such as a
// Google Calendar server or Composio) is a source of kind "mcp": it names
// the integration and a read-only "list events" tool, and maps the tool's
// result to events. Calls go through CallReadOnly, so a tool the server
// doesn't list as read-only is never called.

// MCPSource is a source's integration, tool and mapping (its Config).
type MCPSource struct {
	Integration string `json:"integration"`
	Tool        string `json:"tool"`
	// Args are fixed arguments for every call (a calendar id, say).
	Args map[string]any `json:"args,omitempty"`
	// FromArg and ToArg name the arguments that take the range's start
	// and end (RFC 3339). Empty: the range isn't passed, and events are
	// filtered to it after the call.
	FromArg string `json:"from_arg,omitempty"`
	ToArg   string `json:"to_arg,omitempty"`
	// NextPath is where a result says there are more pages (a token, like
	// Google's nextPageToken), and PageArg the argument that asks for the
	// next one. With NextPath set and no PageArg, more pages are an error:
	// a calendar is never shown with events missing.
	NextPath string `json:"next_path,omitempty"`
	PageArg  string `json:"page_arg,omitempty"`
	// Preset fills in the arguments and mapping for a known server shape
	// ("google": Google Calendar's events.list). Fields set here win.
	Preset string   `json:"preset,omitempty"`
	Map    FieldMap `json:"map,omitzero"`
}

// FieldMap says where an event's fields are in the tool's result. Each is
// a dotted path ("start.dateTime"), or several separated by | to take the
// first that's there. Empty fields use common names (title, summary,
// subject; start, startTime; …).
type FieldMap struct {
	// Items is the path to the list of events in the result ("" = the
	// result itself if it's a list, else items, events, value or data).
	Items    string `json:"items,omitempty"`
	ID       string `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	AllDay   string `json:"all_day,omitempty"`
	Location string `json:"location,omitempty"`
	Link     string `json:"link,omitempty"`
}

// Defaults when a field isn't mapped.
var defaultMap = FieldMap{
	Items:    "items|events|value|data.items|data.events|data|results",
	ID:       "id|uid|iCalUID|event_id",
	Title:    "summary|title|subject|name",
	Start:    "start|startTime|start_time|starts_at|begin",
	End:      "end|endTime|end_time|ends_at|finish",
	AllDay:   "all_day|allDay|isAllDay",
	Location: "location.displayName|location|place",
	Link:     "htmlLink|url|link|webLink|html_link",
}

// mcpPresets are known shapes.
var mcpPresets = map[string]MCPSource{
	// Google Calendar's events.list (what Google Calendar MCP servers and
	// Composio's Google Calendar tools return).
	"google": {FromArg: "timeMin", ToArg: "timeMax", Args: map[string]any{"singleEvents": true, "orderBy": "startTime"},
		NextPath: "nextPageToken|data.nextPageToken", PageArg: "pageToken",
		Map: FieldMap{Items: "items|events|data.items|data.events", ID: "id", Title: "summary", Start: "start", End: "end", Location: "location", Link: "htmlLink"}},
	// Microsoft Graph's calendarView (Outlook servers).
	"outlook": {FromArg: "startDateTime", ToArg: "endDateTime", NextPath: "@odata.nextLink|data.@odata.nextLink",
		Map: FieldMap{Items: "value|events|data.value", ID: "id|iCalUId", Title: "subject", Start: "start", End: "end", AllDay: "isAllDay", Location: "location.displayName", Link: "webLink"}},
}

// MCPPresets lists the known presets.
func MCPPresets() []string { return []string{"google", "outlook"} }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)

// ParseMCPSource reads and checks a source's config, with its preset
// applied.
func ParseMCPSource(config string) (*MCPSource, error) {
	var m MCPSource
	if err := json.Unmarshal([]byte(config), &m); err != nil {
		return nil, fmt.Errorf("unreadable integration calendar settings: %v", err)
	}
	if err := m.check(); err != nil {
		return nil, err
	}
	return m.withPreset(), nil
}

func (m *MCPSource) check() error {
	if !namePattern.MatchString(m.Integration) {
		return errors.New("name the integration (as connected, e.g. gcal)")
	}
	if !namePattern.MatchString(m.Tool) {
		return errors.New("name the integration's read-only tool that lists events")
	}
	if _, ok := mcpPresets[m.Preset]; m.Preset != "" && !ok {
		return fmt.Errorf("unknown preset %q (known: %s; or leave it out and map the fields)", m.Preset, strings.Join(MCPPresets(), ", "))
	}
	for _, a := range []string{m.FromArg, m.ToArg, m.PageArg} {
		if a != "" && !namePattern.MatchString(a) {
			return fmt.Errorf("argument name %q isn't valid", a)
		}
	}
	return nil
}

// withPreset is the source with its preset's settings under its own.
func (m MCPSource) withPreset() *MCPSource {
	p, ok := mcpPresets[m.Preset]
	if !ok {
		return &m
	}
	args := map[string]any{}
	for k, v := range p.Args {
		args[k] = v
	}
	for k, v := range m.Args {
		args[k] = v
	}
	m.Args = args
	m.FromArg = or(m.FromArg, p.FromArg)
	m.ToArg = or(m.ToArg, p.ToArg)
	m.NextPath = or(m.NextPath, p.NextPath)
	m.PageArg = or(m.PageArg, p.PageArg)
	pm := p.Map
	m.Map = FieldMap{Items: or(m.Map.Items, pm.Items), ID: or(m.Map.ID, pm.ID), Title: or(m.Map.Title, pm.Title), Start: or(m.Map.Start, pm.Start),
		End: or(m.Map.End, pm.End), AllDay: or(m.Map.AllDay, pm.AllDay), Location: or(m.Map.Location, pm.Location), Link: or(m.Map.Link, pm.Link)}
	return &m
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// MCPCaller calls an integration's tool only if it's listed as read-only,
// returning its text result.
type MCPCaller func(ctx context.Context, integration, tool string, args json.RawMessage) (string, error)

// fetchMCP calls the source's tool for the range and maps the result.
func (s *Service) fetchMCP(ctx context.Context, src *MCPSource, from, to time.Time, loc *time.Location) ([]Event, error) {
	if s.MCP == nil {
		return nil, errors.New("integrations aren't available")
	}
	args := map[string]any{}
	for k, v := range src.Args {
		args[k] = v
	}
	if src.FromArg != "" {
		args[src.FromArg] = from.UTC().Format(time.RFC3339)
	}
	if src.ToArg != "" {
		args[src.ToArg] = to.UTC().Format(time.RFC3339)
	}
	var all []Event
	for page := 0; ; page++ {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		out, err := s.MCP(ctx, src.Integration, src.Tool, b)
		if err != nil {
			return nil, err
		}
		if len(out) > maxCalendarBytes {
			return nil, errors.New("the result is larger than 8 MB")
		}
		evs, err := mapEvents(out, src.Map, loc, from, to)
		if err != nil {
			return nil, err
		}
		if all = append(all, evs...); len(all) > maxMCPEvents {
			return nil, fmt.Errorf("more than %d events in the range; pass the range to the tool (from_arg and to_arg) so it returns fewer", maxMCPEvents)
		}
		next := nextPage(out, src.NextPath)
		switch {
		case next == "":
			return all, nil
		case src.PageArg == "":
			return nil, errors.New("the tool returned only the first page of events and can't be asked for the next; set page_arg, or have it return all of them")
		case page+1 >= maxMCPPages:
			return nil, fmt.Errorf("more than %d pages of events in the range; not shown, so none go missing", maxMCPPages)
		}
		args[src.PageArg] = next
	}
}

// maxMCPPages is how many pages one range may take.
const maxMCPPages = 20

// nextPage is the token (or link) at path in a result, or "" for none.
func nextPage(result, path string) string {
	if path == "" {
		return ""
	}
	var root any
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &root) != nil {
		return ""
	}
	if v, ok := lookup(root, path); ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// MapEvents turns a tool's JSON result into events, using m (unmapped
// fields use common names). Events without a title or a readable start
// are skipped.
func MapEvents(result string, m FieldMap, loc *time.Location) ([]Event, error) {
	return mapEvents(result, m, loc, time.Time{}, time.Time{})
}

// maxMCPEvents is how many events one call may put on the calendar.
const maxMCPEvents = 5000

// mapEvents is MapEvents keeping only events in [from, to) (all of them
// when from is zero). The cap counts only those, and is an error when
// reached, never a silently shorter calendar.
func mapEvents(result string, m FieldMap, loc *time.Location, from, to time.Time) ([]Event, error) {
	var root any
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(result)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("the tool didn't return JSON: %s", oneLineErr(result))
	}
	items, ok := root.([]any)
	if !ok {
		v, found := lookup(root, or(m.Items, defaultMap.Items))
		if items, ok = v.([]any); !found || !ok {
			return nil, errors.New("couldn't find the list of events in the result; set map.items to its path")
		}
	}
	var out []Event
	readable := 0
	for i, it := range items {
		ev, ok, err := mapEvent(it, m, loc)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		readable++
		if !from.IsZero() && len(filterRange([]Event{ev}, from, to)) == 0 {
			continue
		}
		if len(out) >= maxMCPEvents {
			return nil, fmt.Errorf("more than %d events in the range; pass the range to the tool (from_arg and to_arg) so it returns fewer", maxMCPEvents)
		}
		if ev.UID == "" {
			ev.UID = fmt.Sprintf("item%d", i)
		}
		out = append(out, ev)
	}
	if len(items) > 0 && readable == 0 {
		return nil, errors.New("none of the results had a title and a start time it could read; check the field mapping")
	}
	return out, nil
}

func mapEvent(it any, m FieldMap, loc *time.Location) (Event, bool, error) {
	str := func(path, def string) string {
		v, _ := lookup(it, or(path, def))
		switch x := v.(type) {
		case string:
			return strings.TrimSpace(x)
		case json.Number:
			return x.String()
		}
		return ""
	}
	var ev Event
	ev.Title = str(m.Title, defaultMap.Title)
	startV, _ := lookup(it, or(m.Start, defaultMap.Start))
	start, dateOnly, ok, err := parseWhen(startV, loc)
	if err != nil {
		return ev, false, err
	}
	if ev.Title == "" || !ok {
		return ev, false, nil
	}
	ev.Start, ev.AllDay = start, dateOnly
	if v, found := lookup(it, or(m.AllDay, defaultMap.AllDay)); found {
		if b, isBool := v.(bool); isBool && b {
			ev.AllDay = true
		}
	}
	endV, _ := lookup(it, or(m.End, defaultMap.End))
	end, _, ok, err := parseWhen(endV, loc)
	if err != nil {
		return ev, false, err
	}
	if ok && end.After(start) {
		ev.End = end
	} else if ev.AllDay {
		ev.End = start.AddDate(0, 0, 1)
	}
	ev.UID = str(m.ID, defaultMap.ID)
	ev.Location = str(m.Location, defaultMap.Location)
	if link := str(m.Link, defaultMap.Link); strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://") {
		ev.URL = link
	}
	ev.Title = truncateRunes(ev.Title, 300)
	ev.Location = truncateRunes(ev.Location, 300)
	return ev, true, nil
}

// parseWhen reads a time: a string (RFC 3339, a local date-time, or a
// date for an all-day event), a Unix time, or an object like Google's
// {"dateTime": …, "date": …, "timeZone": …}.
// An explicit time zone that can't be resolved is an error, never
// silently the viewer's.
func parseWhen(v any, loc *time.Location) (t time.Time, dateOnly, ok bool, err error) {
	switch x := v.(type) {
	case string:
		t, dateOnly, ok = parseTimeString(x, loc)
		return t, dateOnly, ok, nil
	case json.Number:
		n, err := x.Int64()
		if err != nil || n <= 0 {
			return time.Time{}, false, false, nil
		}
		if n > 1e12 { // milliseconds
			n /= 1000
		}
		return time.Unix(n, 0).In(loc), false, true, nil
	case map[string]any:
		if tz, isStr := x["timeZone"].(string); isStr && strings.TrimSpace(tz) != "" {
			l, err := zoneNamed(tz)
			if err != nil {
				return time.Time{}, false, false, err
			}
			loc = l
		}
		for _, k := range []string{"dateTime", "date_time", "datetime", "date"} {
			if s, isStr := x[k].(string); isStr && s != "" {
				t, dateOnly, ok = parseTimeString(s, loc)
				return t, dateOnly, ok, nil
			}
		}
	}
	return time.Time{}, false, false, nil
}

// graphZones maps the Windows time zone names Microsoft Graph uses (lower
// case) to IANA names: the common ones; others can be given as IANA names.
var graphZones = map[string]string{
	"utc": "UTC", "coordinated universal time": "UTC", "gmt standard time": "Europe/London", "greenwich standard time": "Atlantic/Reykjavik",
	"w. europe standard time": "Europe/Berlin", "romance standard time": "Europe/Paris", "central europe standard time": "Europe/Budapest",
	"central european standard time": "Europe/Warsaw", "e. europe standard time": "Europe/Chisinau", "fle standard time": "Europe/Kiev",
	"gtb standard time": "Europe/Bucharest", "russian standard time": "Europe/Moscow", "turkey standard time": "Europe/Istanbul",
	"israel standard time": "Asia/Jerusalem", "south africa standard time": "Africa/Johannesburg", "arabian standard time": "Asia/Dubai",
	"india standard time": "Asia/Kolkata", "china standard time": "Asia/Shanghai", "singapore standard time": "Asia/Singapore",
	"tokyo standard time": "Asia/Tokyo", "korea standard time": "Asia/Seoul", "aus eastern standard time": "Australia/Sydney",
	"e. australia standard time": "Australia/Brisbane", "cen. australia standard time": "Australia/Adelaide", "w. australia standard time": "Australia/Perth",
	"new zealand standard time": "Pacific/Auckland", "hawaiian standard time": "Pacific/Honolulu", "alaskan standard time": "America/Anchorage",
	"pacific standard time": "America/Los_Angeles", "us mountain standard time": "America/Phoenix", "mountain standard time": "America/Denver",
	"central standard time": "America/Chicago", "eastern standard time": "America/New_York", "atlantic standard time": "America/Halifax",
	"newfoundland standard time": "America/St_Johns", "canada central standard time": "America/Regina", "sa pacific standard time": "America/Bogota",
	"central standard time (mexico)": "America/Mexico_City", "e. south america standard time": "America/Sao_Paulo", "argentina standard time": "America/Buenos_Aires",
}

// zoneNamed resolves an IANA or a Windows time zone name.
func zoneNamed(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if iana, ok := graphZones[strings.ToLower(name)]; ok {
		name = iana
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q in the result", name)
	}
	return l, nil
}

func parseTimeString(s string, loc *time.Location) (time.Time, bool, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, false, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, false, true
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, true
	}
	return time.Time{}, false, false
}

// lookup follows the first of several dotted paths (a|b.c) that exists.
func lookup(v any, paths string) (any, bool) {
	for _, p := range strings.Split(paths, "|") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if cur, ok := lookupPath(v, p); ok && cur != nil {
			return cur, true
		}
	}
	return nil, false
}

// lookupPath follows one dotted path. A key may itself contain dots
// (Microsoft Graph's "@odata.nextLink"), at any level: the whole rest of
// the path is tried as a key first, then each split at a dot.
func lookupPath(v any, path string) (any, bool) {
	obj, isObj := v.(map[string]any)
	if !isObj {
		return nil, false
	}
	if cur, ok := obj[path]; ok {
		return cur, true
	}
	for i := 0; i < len(path); i++ {
		if path[i] != '.' {
			continue
		}
		if next, ok := obj[path[:i]]; ok {
			if cur, ok := lookupPath(next, path[i+1:]); ok {
				return cur, true
			}
		}
	}
	return nil, false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func oneLineErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, 160)
}
