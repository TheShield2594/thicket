package sprigpkg

import (
	"encoding/json"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

// A shared skill's inputs and success check travel in a package, and a
// package without them still reads the same.
func TestPackageSkillKeepsTaughtFields(t *testing.T) {
	k := store.Skill{ID: "trip-prep", Name: "Trip", Description: "d", Body: "1. Pack",
		Inputs: []store.SkillInput{{Name: "destination", Required: true}}, SuccessCheck: "A checklist is agreed."}
	b, _ := json.Marshal(SkillFrom(k))
	var back Skill
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := back.Store()
	if len(got.Inputs) != 1 || got.Inputs[0] != k.Inputs[0] || got.SuccessCheck != k.SuccessCheck {
		t.Fatalf("round trip: %+v", got)
	}
	if !sameSkill(back, &k) {
		t.Fatal("same skill not recognised")
	}
	changed := k
	changed.SuccessCheck = "something else"
	if sameSkill(back, &changed) {
		t.Fatal("a changed success check counted as the same")
	}
	var old Skill
	if err := json.Unmarshal([]byte(`{"id":"x-y","name":"n","description":"d","body":"1. a"}`), &old); err != nil || old.Inputs != nil || old.SuccessCheck != "" {
		t.Fatalf("format-1 skill: %+v %v", old, err)
	}
}
