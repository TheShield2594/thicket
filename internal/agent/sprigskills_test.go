package agent

import (
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

func TestSkillExtrasRoundTrip(t *testing.T) {
	k := store.Skill{ID: "trip-prep", Name: "Trip prep", Description: "Use when a trip is coming up.", Body: "1. Pack",
		Inputs: []store.SkillInput{{Name: "destination", Description: "where to", Required: true}, {Name: "nights"}}, SuccessCheck: "A checklist is agreed."}
	b, err := FormatSkill(k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSkill(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 2 || got.Inputs[0] != k.Inputs[0] || got.SuccessCheck != k.SuccessCheck {
		t.Fatalf("round trip: %+v\n%s", got, b)
	}
	// A plain skill file has neither field, as before.
	if b, _ := FormatSkill(store.Skill{ID: "x-y", Name: "n", Description: "d", Body: "1. a"}); strings.Contains(string(b), "inputs") || strings.Contains(string(b), "success_check") {
		t.Fatalf("plain skill file: %s", b)
	}
}

func TestSkillExtrasValidated(t *testing.T) {
	base := store.Skill{ID: "trip-prep", Name: "Trip prep", Description: "Use when a trip is coming up.", Body: "1. Pack"}
	for name, mod := range map[string]func(*store.Skill){
		"bad input name":  func(k *store.Skill) { k.Inputs = []store.SkillInput{{Name: "Where To"}} },
		"duplicate input": func(k *store.Skill) { k.Inputs = []store.SkillInput{{Name: "a"}, {Name: "a"}} },
		"too many inputs": func(k *store.Skill) {
			for i := range 11 {
				k.Inputs = append(k.Inputs, store.SkillInput{Name: "in" + string(rune('a'+i))})
			}
		},
		"long check":        func(k *store.Skill) { k.SuccessCheck = strings.Repeat("x", 301) },
		"secret in a check": func(k *store.Skill) { k.SuccessCheck = "the password is hunter2" },
	} {
		k := base
		mod(&k)
		if err := ValidateSkill(k); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateSkill(base); err != nil {
		t.Fatal(err)
	}
}
