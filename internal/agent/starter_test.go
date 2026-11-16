package agent

import (
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

func TestStarterSkills(t *testing.T) {
	pack, err := StarterSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) < 5 {
		t.Fatalf("starter pack has %d skills", len(pack))
	}
	for _, k := range pack {
		// The examples are what people copy, so they follow the advice.
		if h := SkillHints(k); len(h) > 0 {
			t.Errorf("%s: %v", k.ID, h)
		}
		for _, part := range []string{"Needs:", "Done when:", "Watch out:"} {
			if !strings.Contains(k.Body, part) {
				t.Errorf("%s: no %q section", k.ID, part)
			}
		}
		// They must survive an export and re-import unchanged.
		b, err := FormatSkill(k)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ParseSkill(b); err != nil || got.Body != k.Body || got.Description != k.Description {
			t.Errorf("%s: round trip: %v", k.ID, err)
		}
	}
}

func TestSkillHints(t *testing.T) {
	good := store.Skill{ID: "x-skill", Name: "X", Description: "Use when asked for X.", Body: "1. Do it.\n2. Check it."}
	if h := SkillHints(good); len(h) != 0 {
		t.Fatalf("good skill got hints: %v", h)
	}
	vague := store.Skill{ID: "x-skill", Name: "X", Description: "Morning stuff.", Body: "Do the morning things.\n" + strings.Repeat("more ", 700)}
	h := strings.Join(SkillHints(vague), "\n")
	for _, want := range []string{"when to use it", "numbered list", "splitting"} {
		if !strings.Contains(h, want) {
			t.Errorf("missing hint %q in:\n%s", want, h)
		}
	}
}
