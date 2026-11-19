package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Relationship memory is how to be with the person, apart from the facts
// a sprig knows, and an edit shows in the very next prompt.
func TestRelationshipMemoryInPrompt(t *testing.T) {
	c := &character.Character{ID: "juniper", Name: "Juni", Role: "Host."}
	u := &store.User{ID: 1, Name: "Sam", Role: store.RoleOwner}
	mems := []store.Memory{
		{ID: 1, Kind: "fact", Content: "Has a dog named Biscuit."},
		{ID: 2, Character: "juniper", Kind: "tone", Content: "Prefers short answers, no emoji."},
		{ID: 3, Character: "juniper", Kind: "dislikes", Content: "Got annoyed when asked the same thing twice."},
		{ID: 4, Character: "juniper", Kind: "inside_joke", Content: "Calls the office plant Gerald."},
	}
	prompt := func() string {
		return buildSystemPrompt(promptInput{char: c, user: u, roster: []*character.Character{c}, memories: mems, now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)})
	}
	p := prompt()
	rel, facts := strings.Index(p, "## How to be with this person"), strings.Index(p, "## What you remember")
	if rel < 0 || facts < 0 {
		t.Fatalf("sections missing:\n%s", p)
	}
	relPart, factPart := p[rel:facts], p[facts:]
	for _, want := range []string{"How they like you to talk to them:\n  - Prefers short answers, no emoji. (#2)", "What has annoyed them", "Calls the office plant Gerald.", "Don't recite it"} {
		if !strings.Contains(relPart, want) {
			t.Errorf("relationship section missing %q:\n%s", want, relPart)
		}
	}
	if strings.Contains(relPart, "Biscuit") || !strings.Contains(factPart, "Biscuit") {
		t.Fatalf("facts and relationship memory mixed up:\n%s", p)
	}
	if strings.Contains(factPart, "Prefers short answers") {
		t.Fatal("relationship memory listed as a fact to recite")
	}

	// The person edits it on the memory screen: the next prompt has the edit.
	mems[1].Content = "Likes long, detailed answers with examples."
	p = prompt()
	if strings.Contains(p, "Prefers short answers") || !strings.Contains(p, "  - Likes long, detailed answers with examples. (#2)") {
		t.Fatalf("edit not reflected:\n%s", p)
	}

	// No relationship memory yet: no section.
	mems = mems[:1]
	if p = prompt(); strings.Contains(p, "How to be with this person") {
		t.Fatal("empty relationship section")
	}
}
