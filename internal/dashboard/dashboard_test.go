package dashboard

import (
	"strings"
	"testing"
)

// What a person approves must be exactly what runs: no re-encoding, so
// large numbers aren't rounded.
func TestArgumentsShownExactly(t *testing.T) {
	if got := prettyJSON(`{"n":9007199254740993}`); !strings.Contains(got, "9007199254740993") {
		t.Fatalf("number changed: %s", got)
	}
	got, ok := draftArgs(`{"tool":"mcp_ha__set","args":{"n":9007199254740993},"chat_ref":"discord:x"}`)
	if !ok || !strings.HasPrefix(got, "mcp_ha__set\n") || !strings.Contains(got, "9007199254740993") {
		t.Fatalf("draft: %q", got)
	}
	if _, ok := draftArgs(""); ok {
		t.Fatal("a draft without an action can't be shown as runnable")
	}
}
