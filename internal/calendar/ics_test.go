package calendar

import (
	"strings"
	"testing"
	"time"
)

const sample = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
	// A one-off in New York time, with a folded summary and an alarm.
	"BEGIN:VEVENT\r\nUID:dentist\r\nSUMMARY:Dentist\\, then gro\r\n ceries\r\nDTSTART;TZID=America/New_York:20261120T093000\r\nDTEND;TZID=America/New_York:20261120T103000\r\nLOCATION:Main St\r\nBEGIN:VALARM\r\nTRIGGER:-PT15M\r\nSUMMARY:not the event\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
	// All day.
	"BEGIN:VEVENT\r\nUID:holiday\r\nSUMMARY:School holiday\r\nDTSTART;VALUE=DATE:20261123\r\nDTEND;VALUE=DATE:20261124\r\nEND:VEVENT\r\n" +
	// Weekly on Mon and Wed, UTC, 6 times, minus one, with one moved.
	"BEGIN:VEVENT\r\nUID:swim\r\nSUMMARY:Swimming\r\nDTSTART:20261116T170000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE;COUNT=6\r\nEXDATE:20261118T170000Z\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:swim\r\nRECURRENCE-ID:20261123T170000Z\r\nSUMMARY:Swimming (moved)\r\nDTSTART:20261124T170000Z\r\nDTEND:20261124T180000Z\r\nEND:VEVENT\r\n" +
	// Monthly on the last Friday.
	"BEGIN:VEVENT\r\nUID:book\r\nSUMMARY:Book club\r\nDTSTART:20261030T190000Z\r\nDTEND:20261030T200000Z\r\nRRULE:FREQ=MONTHLY;BYDAY=-1FR\r\nEND:VEVENT\r\n" +
	// Cancelled.
	"BEGIN:VEVENT\r\nUID:gone\r\nSUMMARY:Cancelled thing\r\nSTATUS:CANCELLED\r\nDTSTART:20261121T120000Z\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParseICS(t *testing.T) {
	from := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 45)
	evs, err := ParseICS(sample, from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Start.UTC().Format("01-02 15:04")+" "+e.Title)
	}
	want := []string{
		"11-16 17:00 Swimming",
		"11-20 14:30 Dentist, then groceries", // 09:30 New York
		"11-23 00:00 School holiday",
		"11-24 17:00 Swimming (moved)", // replaces the 23rd
		"11-25 17:00 Swimming",
		"11-27 19:00 Book club", // last Friday of November
		"11-30 17:00 Swimming",  // the 18th was excluded; COUNT counts it, so 6 = 16,18,23,25,30,2
		"12-02 17:00 Swimming",
		"12-25 19:00 Book club",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range evs {
		if e.Title == "School holiday" && (!e.AllDay || e.End.Sub(e.Start) != 24*time.Hour) {
			t.Errorf("all-day event: %+v", e)
		}
		if strings.HasPrefix(e.Title, "Dentist") && e.Location != "Main St" {
			t.Errorf("location: %+v", e)
		}
	}
}

func TestExpandRules(t *testing.T) {
	start := time.Date(2026, 1, 31, 8, 0, 0, 0, time.UTC)
	until := start.AddDate(1, 0, 0)
	// Monthly on the 31st skips short months.
	got, _ := expand(start, "FREQ=MONTHLY", until)
	if len(got) != 8 { // Jan, Mar, May, Jul, Aug, Oct, Dec, and Jan again (the range end is inclusive)
		t.Fatalf("monthly on the 31st: %d %v", len(got), got)
	}
	// Daily every 2 days until a date.
	got, _ = expand(start, "FREQ=DAILY;INTERVAL=2;UNTIL=20260206", until)
	if len(got) != 4 { // 31, 2, 4, 6
		t.Fatalf("daily interval until: %v", got)
	}
	// Yearly on 29 February only in leap years.
	leap := time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC)
	got, _ = expand(leap, "FREQ=YEARLY;COUNT=2", leap.AddDate(10, 0, 0))
	if len(got) != 2 || got[1].Year() != 2032 {
		t.Fatalf("leap day: %v", got)
	}
	// Unbounded rules stop at the range.
	got, _ = expand(start, "FREQ=DAILY", start.AddDate(0, 0, 10))
	if len(got) != 11 {
		t.Fatalf("bounded by range: %d", len(got))
	}
}
