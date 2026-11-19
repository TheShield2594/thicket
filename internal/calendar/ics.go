// Package calendar is Thicket's household calendar: the calendars adults
// connect (ICS feeds and CalDAV, read-only), the jobs sprigs run, and the
// things sprigs put on it, merged into one view with filters per layer
// and per sprig.
package calendar

import (
	"bufio"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Event is one occurrence from a connected calendar.
type Event struct {
	UID      string
	Title    string
	Start    time.Time
	End      time.Time
	AllDay   bool
	Location string
	URL      string
}

// maxOccurrences bounds how far a recurring event is expanded.
const maxOccurrences = 1000

type icsProp struct {
	name   string
	params map[string]string
	value  string
}

// unfold joins RFC 5545 folded lines.
func unfold(data string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(data, "\r\n", "\n")))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

func parseProp(line string) (icsProp, bool) {
	// The value starts at the first colon outside a quoted parameter.
	inQuote, colon := false, -1
	for i, r := range line {
		if r == '"' {
			inQuote = !inQuote
		} else if r == ':' && !inQuote {
			colon = i
			break
		}
	}
	if colon < 0 {
		return icsProp{}, false
	}
	head, value := line[:colon], line[colon+1:]
	parts := strings.Split(head, ";")
	p := icsProp{name: strings.ToUpper(parts[0]), params: map[string]string{}, value: value}
	for _, kv := range parts[1:] {
		k, v, _ := strings.Cut(kv, "=")
		p.params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	return p, true
}

func unescape(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(s)
}

// parseTime reads a DATE or DATE-TIME property. Floating times use loc.
func parseTime(p icsProp, loc *time.Location) (time.Time, bool, error) {
	v := strings.TrimSpace(p.value)
	if p.params["VALUE"] == "DATE" || (len(v) == 8 && !strings.Contains(v, "T")) {
		t, err := time.ParseInLocation("20060102", v, loc)
		return t, true, err
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		return t, false, err
	}
	in := loc
	if tz := p.params["TZID"]; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			in = l
		} else if l, ok := windowsZones[tz]; ok {
			in = l
		}
	}
	t, err := time.ParseInLocation("20060102T150405", v, in)
	return t, false, err
}

// A few zone names Outlook and Exchange use instead of IANA names.
var windowsZones = map[string]*time.Location{}

func init() {
	for win, iana := range map[string]string{
		"Pacific Standard Time": "America/Los_Angeles", "Mountain Standard Time": "America/Denver",
		"Central Standard Time": "America/Chicago", "Eastern Standard Time": "America/New_York",
		"GMT Standard Time": "Europe/London", "W. Europe Standard Time": "Europe/Berlin",
		"Romance Standard Time": "Europe/Paris", "Central Europe Standard Time": "Europe/Budapest",
		"AUS Eastern Standard Time": "Australia/Sydney", "Tokyo Standard Time": "Asia/Tokyo",
	} {
		if l, err := time.LoadLocation(iana); err == nil {
			windowsZones[win] = l
		}
	}
}

type vevent struct {
	uid, summary, location, url, status string
	start, end                          time.Time
	allDay                              bool
	duration                            time.Duration
	rrule                               string
	exdates                             map[int64]bool
	recurrenceID                        time.Time // set on an overridden occurrence
	extra                               []time.Time
}

// ParseICS returns the occurrences of the calendar's events that overlap
// [from, to). Floating and all-day times are read in loc.
func ParseICS(data string, from, to time.Time, loc *time.Location) ([]Event, error) {
	if loc == nil {
		loc = time.UTC
	}
	var evs []*vevent
	var cur *vevent
	depth := 0 // nested components inside a VEVENT (VALARM) are skipped
	for _, line := range unfold(data) {
		p, ok := parseProp(line)
		if !ok {
			continue
		}
		switch {
		case p.name == "BEGIN" && strings.EqualFold(p.value, "VEVENT"):
			cur, depth = &vevent{exdates: map[int64]bool{}}, 0
			continue
		case p.name == "END" && strings.EqualFold(p.value, "VEVENT"):
			if cur != nil {
				evs = append(evs, cur)
			}
			cur = nil
			continue
		case cur == nil:
			continue
		case p.name == "BEGIN":
			depth++
			continue
		case p.name == "END":
			depth--
			continue
		case depth > 0:
			continue
		}
		switch p.name {
		case "UID":
			cur.uid = p.value
		case "SUMMARY":
			cur.summary = unescape(p.value)
		case "LOCATION":
			cur.location = unescape(p.value)
		case "URL":
			cur.url = p.value
		case "STATUS":
			cur.status = strings.ToUpper(p.value)
		case "DTSTART":
			t, allDay, err := parseTime(p, loc)
			if err != nil {
				return nil, fmt.Errorf("event %q: bad start: %v", cur.summary, err)
			}
			cur.start, cur.allDay = t, allDay
		case "DTEND":
			if t, _, err := parseTime(p, loc); err == nil {
				cur.end = t
			}
		case "DURATION":
			cur.duration = parseDuration(p.value)
		case "RRULE":
			cur.rrule = p.value
		case "RDATE":
			for _, v := range strings.Split(p.value, ",") {
				if t, _, err := parseTime(icsProp{params: p.params, value: v}, loc); err == nil {
					cur.extra = append(cur.extra, t)
				}
			}
		case "EXDATE":
			for _, v := range strings.Split(p.value, ",") {
				if t, _, err := parseTime(icsProp{params: p.params, value: v}, loc); err == nil {
					cur.exdates[t.Unix()] = true
				}
			}
		case "RECURRENCE-ID":
			if t, _, err := parseTime(p, loc); err == nil {
				cur.recurrenceID = t
			}
		}
	}
	// Overridden occurrences replace the series' instance at that time.
	overridden := map[string]map[int64]bool{}
	for _, v := range evs {
		if !v.recurrenceID.IsZero() {
			if overridden[v.uid] == nil {
				overridden[v.uid] = map[int64]bool{}
			}
			overridden[v.uid][v.recurrenceID.Unix()] = true
		}
	}
	var out []Event
	for _, v := range evs {
		if v.start.IsZero() || v.status == "CANCELLED" {
			continue
		}
		length := v.end.Sub(v.start)
		if v.end.IsZero() {
			length = v.duration
			if length == 0 && v.allDay {
				length = 24 * time.Hour
			}
		}
		starts := []time.Time{v.start}
		if v.rrule != "" && v.recurrenceID.IsZero() {
			var err error
			if starts, err = expand(v.start, v.rrule, to); err != nil {
				return nil, fmt.Errorf("event %q: %v", v.summary, err)
			}
		}
		starts = append(starts, v.extra...)
		for _, s := range starts {
			if v.exdates[s.Unix()] || (v.recurrenceID.IsZero() && overridden[v.uid][s.Unix()]) {
				continue
			}
			end := s.Add(length)
			if !s.Before(to) || !end.After(from) && !(length == 0 && !s.Before(from)) {
				continue
			}
			out = append(out, Event{UID: v.uid, Title: v.summary, Start: s, End: end, AllDay: v.allDay, Location: v.location, URL: v.url})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// parseDuration reads an RFC 5545 duration such as PT1H30M or P1D.
func parseDuration(s string) time.Duration {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "+"), "P")
	var d time.Duration
	num := ""
	inTime := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'T':
			inTime = true
		default:
			n, _ := strconv.Atoi(num)
			num = ""
			switch {
			case r == 'W':
				d += time.Duration(n) * 7 * 24 * time.Hour
			case r == 'D':
				d += time.Duration(n) * 24 * time.Hour
			case r == 'H' && inTime:
				d += time.Duration(n) * time.Hour
			case r == 'M' && inTime:
				d += time.Duration(n) * time.Minute
			case r == 'S' && inTime:
				d += time.Duration(n) * time.Second
			}
		}
	}
	return d
}

var weekdays = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

// expand lists a recurring event's starts up to `to`. It covers the rules
// calendars commonly write: DAILY, WEEKLY (with BYDAY), MONTHLY (by day of
// month, or BYDAY like 2TU / -1FR), YEARLY, with INTERVAL, COUNT and UNTIL.
func expand(start time.Time, rule string, to time.Time) ([]time.Time, error) {
	r := map[string]string{}
	for _, kv := range strings.Split(rule, ";") {
		k, v, _ := strings.Cut(kv, "=")
		r[strings.ToUpper(k)] = strings.ToUpper(v)
	}
	interval := 1
	if v, err := strconv.Atoi(r["INTERVAL"]); err == nil && v > 0 {
		interval = v
	}
	count := -1
	if v, err := strconv.Atoi(r["COUNT"]); err == nil {
		count = v
	}
	until := to
	if u := r["UNTIL"]; u != "" {
		p := icsProp{value: u, params: map[string]string{}}
		if t, _, err := parseTime(p, start.Location()); err == nil && t.Before(until) {
			until = t
			if len(u) == 8 { // a DATE: the whole day counts
				until = t.Add(24*time.Hour - time.Second)
			}
		}
	}
	var out []time.Time
	add := func(t time.Time) bool {
		if t.Before(start) {
			return true
		}
		if t.After(until) || count == 0 || len(out) >= maxOccurrences {
			return false
		}
		out = append(out, t)
		if count > 0 {
			count--
		}
		return true
	}
	at := func(day time.Time) time.Time { // keep the start's wall-clock time
		return time.Date(day.Year(), day.Month(), day.Day(), start.Hour(), start.Minute(), start.Second(), 0, start.Location())
	}
	var byday []string
	if r["BYDAY"] != "" {
		byday = strings.Split(r["BYDAY"], ",")
	}
	switch r["FREQ"] {
	case "DAILY":
		for t := start; add(t); t = t.AddDate(0, 0, interval) {
		}
	case "WEEKLY":
		days := map[time.Weekday]bool{}
		for _, d := range byday {
			if wd, ok := weekdays[d[max(0, len(d)-2):]]; ok {
				days[wd] = true
			}
		}
		if len(days) == 0 {
			days[start.Weekday()] = true
		}
		weekStart := at(start).AddDate(0, 0, -int(start.Weekday())) // Sunday of the first week
		for w := weekStart; ; w = w.AddDate(0, 0, 7*interval) {
			ok := true
			for i := 0; i < 7 && ok; i++ {
				d := w.AddDate(0, 0, i)
				if days[d.Weekday()] {
					ok = add(at(d))
				}
			}
			if !ok || w.After(until) {
				break
			}
		}
	case "MONTHLY":
		for m := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location()); ; m = m.AddDate(0, interval, 0) {
			var days []time.Time
			if len(byday) > 0 {
				for _, d := range byday {
					days = append(days, nthWeekday(m, d)...)
				}
			} else if day := start.Day(); day <= daysIn(m) {
				days = []time.Time{time.Date(m.Year(), m.Month(), day, 0, 0, 0, 0, m.Location())}
			}
			sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
			ok := true
			for _, d := range days {
				if ok = add(at(d)); !ok {
					break
				}
			}
			if !ok || m.After(until) {
				break
			}
		}
	case "YEARLY":
		for y := 0; ; y += interval {
			t := start.AddDate(y, 0, 0)
			if t.Day() != start.Day() { // 29 February in a common year
				continue
			}
			if !add(t) || t.After(until) {
				break
			}
		}
	default:
		return []time.Time{start}, nil // an unsupported rule: the first occurrence only
	}
	return out, nil
}

func daysIn(m time.Time) int {
	return time.Date(m.Year(), m.Month()+1, 0, 0, 0, 0, 0, m.Location()).Day()
}

// nthWeekday reads a BYDAY entry for a month: "TU" (every Tuesday), "2TU"
// (the second), "-1FR" (the last Friday).
func nthWeekday(month time.Time, spec string) []time.Time {
	if len(spec) < 2 {
		return nil
	}
	wd, ok := weekdays[spec[len(spec)-2:]]
	if !ok {
		return nil
	}
	var all []time.Time
	for d := 1; d <= daysIn(month); d++ {
		t := time.Date(month.Year(), month.Month(), d, 0, 0, 0, 0, month.Location())
		if t.Weekday() == wd {
			all = append(all, t)
		}
	}
	n, err := strconv.Atoi(spec[:len(spec)-2])
	switch {
	case spec[:len(spec)-2] == "":
		return all
	case err != nil || n == 0 || abs(n) > len(all):
		return nil
	case n > 0:
		return all[n-1 : n]
	default:
		return all[len(all)+n : len(all)+n+1]
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
