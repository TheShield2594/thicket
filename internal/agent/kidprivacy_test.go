package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

func TestPersonalDetails(t *testing.T) {
	said := []string{
		"hi! my name is Max Turner and I'm 9",
		"I live on Willowbrook Lane, by the park. Our house is 42 Elm Street",
		"I go to Maple Grove Elementary. mum's number is 555-201-7788, email max.t@example.com",
		"i live in Springfield",
	}
	details := personalDetails(said, "Max")
	for _, want := range []string{"max turner", "turner", "willowbrook lane", "willowbrook", "42 elm street", "maple grove elementary", "maple", "5552017788", "springfield", "max t example com"} {
		found := false
		for _, d := range details {
			found = found || d == want
		}
		if !found {
			t.Errorf("missing %q in %q", want, details)
		}
	}
	for _, d := range details {
		if d == "max" || stopWords[d] {
			t.Errorf("%q isn't a detail", d)
		}
	}

	for _, leak := range []string{
		"things to do in Springfield", "Maple Grove Elementary lunch menu", "https://maps.example/?q=42+Elm+St", "turner family",
		"call 555 201 7788", "+1 (555) 201-7788", "write to someone@school.example", "99 Oak Road", "willowbrook",
	} {
		if !carriesDetails(leak, details) {
			t.Errorf("%q not caught", leak)
		}
	}
	for _, ok := range []string{"how do volcanoes work", "minecraft redstone tips", "what happened on 2026-11-19", "lego set 75192", "max level in zelda", "best park games"} {
		if carriesDetails(ok, details) {
			t.Errorf("%q flagged", ok)
		}
	}
	got := redactDetails("Max talked about Maple Grove Elementary, Springfield and 42 Elm Street; call 555-201-7788.", details)
	for _, leak := range []string{"Maple", "Springfield", "Elm", "7788"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted text still has %q: %s", leak, got)
		}
	}
}

func TestUsageOf(t *testing.T) {
	at := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	msg := func(role string, min int) store.KidChatMessage {
		return store.KidChatMessage{Message: store.Message{Role: role, CreatedAt: at.Add(time.Duration(min) * time.Minute)}}
	}
	// A 12-minute sitting, then one lone message an hour later.
	u := usageOf([]store.KidChatMessage{msg("user", 0), msg("assistant", 1), msg("user", 4), msg("assistant", 5), msg("user", 9), msg("assistant", 12), msg("user", 72)})
	if u.Messages != 4 || u.Minutes != 13 {
		t.Fatalf("usage: %+v", u)
	}
	if u := usageOf(nil); u.Messages != 0 || u.Minutes != 0 {
		t.Fatalf("nothing: %+v", u)
	}
}
