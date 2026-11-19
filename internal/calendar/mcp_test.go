package calendar

import (
	"context"
	"encoding/json"
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

func TestMapEventsZonesAndRange(t *testing.T) {
	// A Windows zone name (Microsoft Graph) is resolved, not ignored.
	evs, err := MapEvents(`{"value":[{"subject":"Call","start":{"dateTime":"2026-11-20T09:00:00","timeZone":"Pacific Standard Time"}}]}`, FieldMap{}, time.UTC)
	if err != nil || len(evs) != 1 || evs[0].Start.UTC().Format(time.RFC3339) != "2026-11-20T17:00:00Z" {
		t.Fatalf("Pacific Standard Time: %v %v", evs, err)
	}
	// An explicit zone that can't be resolved is an error, not the viewer's zone.
	if _, err := MapEvents(`[{"title":"x","start":{"dateTime":"2026-11-20T09:00:00","timeZone":"Mars Standard Time"}}]`, FieldMap{}, time.UTC); err == nil {
		t.Fatal("an unknown zone was ignored")
	}
	// The range applies before the cap: old events never crowd out this week's.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < maxMCPEvents+10; i++ {
		b.WriteString(`{"title":"old","start":"2020-01-01"},`)
	}
	b.WriteString(`{"title":"this week","start":"2026-11-20T10:00:00Z"}]`)
	from, to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	evs, err = mapEvents(b.String(), FieldMap{}, time.UTC, from, to)
	if err != nil || len(evs) != 1 || evs[0].Title != "this week" {
		t.Fatalf("range before cap: %d events, %v", len(evs), err)
	}
	// Too many in range is an error, never a shorter calendar.
	b.Reset()
	b.WriteString("[")
	for i := 0; i <= maxMCPEvents; i++ {
		b.WriteString(`{"title":"busy","start":"2026-11-20T10:00:00Z"},`)
	}
	b.WriteString(`{"title":"last","start":"2026-11-20T11:00:00Z"}]`)
	if _, err := mapEvents(b.String(), FieldMap{}, time.UTC, from, to); err == nil {
		t.Fatal("more events than the cap came back without an error")
	}
}

func TestFetchMCPPages(t *testing.T) {
	from, to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	src, err := ParseMCPSource(`{"integration":"gcal","tool":"list_events","preset":"google"}`)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	s := &Service{MCP: func(ctx context.Context, integration, tool string, args json.RawMessage) (string, error) {
		var a map[string]any
		json.Unmarshal(args, &a)
		tok, _ := a["pageToken"].(string)
		tokens = append(tokens, tok)
		switch tok {
		case "":
			return `{"items":[{"summary":"one","start":{"dateTime":"2026-11-17T10:00:00Z"}}],"nextPageToken":"p2"}`, nil
		case "p2":
			return `{"items":[{"summary":"two","start":{"dateTime":"2026-11-18T10:00:00Z"}}]}`, nil
		}
		return "", nil
	}}
	evs, err := s.fetchMCP(context.Background(), src, from, to, time.UTC)
	if err != nil || len(evs) != 2 || evs[1].Title != "two" || strings.Join(tokens, ",") != ",p2" {
		t.Fatalf("pages: %v %v (tokens %q)", evs, err, tokens)
	}
	// Endless pages: an error, not a partial calendar.
	s.MCP = func(ctx context.Context, integration, tool string, args json.RawMessage) (string, error) {
		return `{"items":[],"nextPageToken":"again"}`, nil
	}
	if _, err := s.fetchMCP(context.Background(), src, from, to, time.UTC); err == nil {
		t.Fatal("endless pages came back without an error")
	}
	// A next page it can't ask for (Outlook's nextLink): an error too.
	out, _ := ParseMCPSource(`{"integration":"o","tool":"list","preset":"outlook"}`)
	s.MCP = func(ctx context.Context, integration, tool string, args json.RawMessage) (string, error) {
		return `{"value":[{"subject":"x","start":{"dateTime":"2026-11-17T10:00:00","timeZone":"UTC"}}],"@odata.nextLink":"https://graph.example/next"}`, nil
	}
	if _, err := s.fetchMCP(context.Background(), out, from, to, time.UTC); err == nil {
		t.Fatal("a missing page was ignored")
	}
	// The same link inside a wrapper object (data.@odata.nextLink).
	s.MCP = func(ctx context.Context, integration, tool string, args json.RawMessage) (string, error) {
		return `{"data":{"value":[],"@odata.nextLink":"https://graph.example/next"}}`, nil
	}
	if _, err := s.fetchMCP(context.Background(), out, from, to, time.UTC); err == nil {
		t.Fatal("a missing page inside data was ignored")
	}
}
