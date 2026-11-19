package calendar

import (
	"strings"
	"testing"
	"time"
)

func TestMapEventsShapes(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		name, result string
		m            FieldMap
		want         string // title@start(UTC)[all-day]|…
	}{
		{"outlook", `{"value":[{"id":"a","subject":"Standup","start":{"dateTime":"2026-11-19T15:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-19T15:15:00.0000000","timeZone":"UTC"},"isAllDay":false,"location":{"displayName":"Room 4"},"webLink":"https://outlook.example/a"}]}`,
			mcpPresets["outlook"].Map, "Standup@2026-11-19T15:00:00Z"},
		{"a bare list with common names", `[{"title":"Dentist","start":"2026-11-20T09:30:00-05:00","end":"2026-11-20T10:30:00-05:00"},{"title":"Holiday","start":"2026-11-26"}]`,
			FieldMap{}, "Dentist@2026-11-20T14:30:00Z|Holiday@2026-11-26T05:00:00Z[all-day]"},
		{"local times in the person's zone, unix times", `{"events":[{"name":"Swim","starts_at":"2026-11-21 08:00"},{"name":"Call","start_time":1795000000}]}`,
			FieldMap{}, "Swim@2026-11-21T13:00:00Z|Call@2026-11-18T11:06:40Z"},
		{"custom paths, skipping what can't be read", `{"data":{"rows":[{"what":"Match","when":{"date":"2026-11-22"}},{"what":"","when":"2026-11-22"},{"what":"No time"}]}}`,
			FieldMap{Items: "data.rows", Title: "what", Start: "when"}, "Match@2026-11-22T05:00:00Z[all-day]"},
	}
	for _, c := range cases {
		evs, err := MapEvents(c.result, c.m, ny)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, e := range evs {
			s := e.Title + "@" + e.Start.UTC().Format(time.RFC3339)
			if e.AllDay {
				s += "[all-day]"
			}
			got = append(got, s)
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%s: got %q, want %q", c.name, strings.Join(got, "|"), c.want)
		}
	}
	evs, _ := MapEvents(cases[0].result, cases[0].m, ny)
	if e := evs[0]; e.Location != "Room 4" || e.URL != "https://outlook.example/a" || e.UID != "a" || e.End.Sub(e.Start) != 15*time.Minute {
		t.Fatalf("outlook fields: %+v", e)
	}
	evs, _ = MapEvents(`[{"title":"x","start":"2026-11-20","link":"javascript:alert(1)"}]`, FieldMap{}, ny)
	if evs[0].URL != "" || !evs[0].End.Equal(evs[0].Start.AddDate(0, 0, 1)) {
		t.Fatalf("unsafe link kept or all-day end missing: %+v", evs[0])
	}

	for _, bad := range []string{`not json`, `{"nothing":"here"}`, `{"items":[{"foo":1}]}`} {
		if _, err := MapEvents(bad, FieldMap{}, ny); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if evs, err := MapEvents(`{"items":[]}`, FieldMap{}, ny); err != nil || len(evs) != 0 {
		t.Fatalf("an empty calendar: %v %v", evs, err)
	}
}

func TestParseMCPSource(t *testing.T) {
	m, err := ParseMCPSource(`{"integration":"gcal","tool":"list_events","preset":"google","args":{"calendarId":"family","orderBy":"updated"},"map":{"title":"title"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if m.FromArg != "timeMin" || m.ToArg != "timeMax" || m.Args["calendarId"] != "family" || m.Args["orderBy"] != "updated" || m.Args["singleEvents"] != true {
		t.Fatalf("preset not applied under its own settings: %+v", m)
	}
	if m.Map.Title != "title" || m.Map.Start != "start" || m.Map.Link != "htmlLink" {
		t.Fatalf("mapping: %+v", m.Map)
	}
	for _, bad := range []string{`{}`, `{"integration":"gcal"}`, `{"integration":"gcal","tool":"x","preset":"nope"}`, `{"integration":"a b","tool":"x"}`, `{"integration":"gcal","tool":"x","from_arg":"a b"}`} {
		if _, err := ParseMCPSource(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
