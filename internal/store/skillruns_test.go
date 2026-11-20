package store

import (
	"context"
	"errors"
	"testing"
)

func TestSprigSkillsAndRuns(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.SaveSkill(ctx, Skill{ID: "shared-one", Name: "S", Description: "d", Body: "1. x"}, 0); err != nil {
		t.Fatal(err)
	}
	own := Skill{ID: "pip-tea", Name: "Tea", Description: "d", Body: "1. x", SprigID: "pip",
		Inputs: []SkillInput{{Name: "kind", Required: true}}, SuccessCheck: "a cup"}
	if _, err := s.SaveSkill(ctx, own, 0); err != nil {
		t.Fatal(err)
	}
	// Same content again is no change; a changed input is a new version
	// that keeps the owner, whatever the revision says.
	if _, err := s.SaveSkill(ctx, own, 1); !errors.Is(err, ErrNoChange) {
		t.Fatalf("same save: %v", err)
	}
	own.Inputs[0].Required, own.SprigID = false, ""
	if v, err := s.SaveSkill(ctx, own, 1); err != nil || v != 2 {
		t.Fatalf("revise: %d %v", v, err)
	}
	if k, _ := s.SkillByID(ctx, "pip-tea", 0); k.SprigID != "pip" || k.Inputs[0].Required {
		t.Fatalf("revised: %+v", k)
	}
	for sprig, want := range map[string]int{"pip": 2, "fern": 1} {
		if ks, _ := s.SkillsFor(ctx, sprig); len(ks) != want {
			t.Fatalf("%s offered %d skills, want %d", sprig, len(ks), want)
		}
	}

	id, err := s.StartSkillRun(ctx, SkillRun{SkillID: "pip-tea", Version: 2, SprigID: "pip", ChatRef: "dm:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSkillRun(ctx, id, "fern", SkillSucceeded, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another sprig finished pip's run: %v", err)
	}
	if n, _ := s.StopSkillRuns(ctx, "pip", "dm:1"); n != 1 {
		t.Fatalf("stopped %d", n)
	}
	if err := s.FinishSkillRun(ctx, id, "pip", SkillSucceeded, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished a stopped run: %v", err)
	}
	runs, _ := s.SkillRuns(ctx, "pip-tea", "", 5)
	if len(runs) != 1 || runs[0].Outcome != SkillStopped {
		t.Fatalf("runs: %+v", runs)
	}
	if n, _ := s.SkillRunCount(ctx, "pip-tea", "pip"); n != 1 {
		t.Fatalf("count %d", n)
	}

	if err := s.PromoteSkill(ctx, "pip-tea"); err != nil {
		t.Fatal(err)
	}
	if ks, _ := s.SkillsFor(ctx, "fern"); len(ks) != 2 {
		t.Fatalf("promoted skill not shared: %+v", ks)
	}
	if err := s.PromoteSkill(ctx, "pip-tea"); err == nil {
		t.Fatal("promoted twice")
	}
}
