package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

const pipTea = `---
id: pip-tea
name: Pip's tea
description: Use when the user wants tea made their way.
inputs:
  - name: kind
    description: which tea
    required: true
success_check: The user has a cup they like.
---
1. Boil water
2. Steep four minutes
`

// A skill in characters/pip/skills/ is offered to Pip only, its runs are
// recorded, and the shared library works as before.
func TestSprigOwnedSkills(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	onboard(t, a, fc, ctx)
	dir := a.Cfg.CharactersDir
	skillDir := character.SkillDir(dir, "pip")

	e.HandleIncoming(ctx, dm("save skill briefing"))
	fc.take()
	if k, err := a.DB.SkillByID(ctx, "briefing", 0); err != nil || !k.Shared() {
		t.Fatalf("an admin's new skill should be shared, as before: %+v %v", k, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "pip.md"), []byte(sprigFile("pip", "Pip", 0)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "pip-tea.md"), []byte(pipTea), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	k, err := a.DB.SkillByID(ctx, "pip-tea", 0)
	if err != nil || k.SprigID != "pip" || k.Version != 1 || len(k.Inputs) != 1 || !k.Inputs[0].Required || k.SuccessCheck == "" {
		t.Fatalf("mirrored skill: %+v %v", k, err)
	}

	// Pip sees its own skill and the shared one; Juni only the shared one.
	e.HandleIncoming(ctx, dm("Pip, hello"))
	fc.take()
	if p := systemPromptOf(lastChatReq(fm)); !strings.Contains(p, "- pip-tea (yours only): Pip's tea.") || !strings.Contains(p, "- briefing: Morning briefing.") {
		t.Fatalf("Pip's index:\n%s", p)
	}
	e.HandleIncoming(ctx, dm("Juniper, hello"))
	fc.take()
	if p := systemPromptOf(lastChatReq(fm)); strings.Contains(p, "pip-tea") || !strings.Contains(p, "- briefing:") {
		t.Fatalf("Juni's index:\n%s", p)
	}
	e.HandleIncoming(ctx, dm("Juniper, use skill pip-tea"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, `there's no skill "pip-tea"`) {
		t.Fatalf("another sprig used Pip's skill: %s", r)
	}

	// Following it records a run, and finishing records the outcome.
	if k, err := a.DB.SkillByID(ctx, "pip-tea", 0); err != nil {
		t.Fatalf("before use: %+v %v", k, err)
	}
	e.HandleIncoming(ctx, dm("Pip, use skill pip-tea"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "Run 1 started (your run number 1") || !strings.Contains(r, "- kind (required): which tea") ||
		!strings.Contains(r, "Succeeded when: The user has a cup they like.") {
		t.Fatalf("use_skill: %s", r)
	}
	if n, _ := a.DB.SkillRunCount(ctx, "pip-tea", "pip"); n != 1 {
		t.Fatalf("run count %d", n)
	}
	e.HandleIncoming(ctx, dm("Pip, finish skill run 1 succeeded"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "Run 1 recorded as succeeded") {
		t.Fatalf("finish_skill: %s", r)
	}
	runs, _ := a.DB.SkillRuns(ctx, "pip-tea", "pip", 10)
	if len(runs) != 1 || runs[0].Outcome != store.SkillSucceeded || runs[0].FinishedAt.IsZero() || runs[0].Note != "went fine" {
		t.Fatalf("runs: %+v", runs)
	}
	e.HandleIncoming(ctx, dm("Pip, finish skill run 1 failed"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "no run 1 still going") {
		t.Fatalf("finished twice: %s", r)
	}
	// Shared skills are followed and counted the same way.
	e.HandleIncoming(ctx, dm("Juniper, use skill briefing"))
	fc.take()
	if n, _ := a.DB.SkillRunCount(ctx, "briefing", "juniper"); n != 1 {
		t.Fatalf("shared skill run count %d", n)
	}

	// The folder is the source of truth: an edit is a new version, a
	// clash with the shared library is skipped, a removed file deletes it.
	if err := os.WriteFile(filepath.Join(skillDir, "pip-tea.md"), []byte(strings.Replace(pipTea, "four", "five", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	clash := strings.NewReplacer("id: pip-tea", "id: briefing").Replace(pipTea)
	if err := os.WriteFile(filepath.Join(skillDir, "briefing.md"), []byte(clash), 0o644); err != nil {
		t.Fatal(err)
	}
	probs := e.SyncSprigSkills(ctx)
	if len(probs) != 1 || !strings.Contains(probs[0].Error(), "shared library already has a skill with the id \"briefing\"") {
		t.Fatalf("problems: %v", probs)
	}
	if k, _ := a.DB.SkillByID(ctx, "pip-tea", 0); k.Version != 2 || !strings.Contains(k.Body, "five") {
		t.Fatalf("edited file: %+v", k)
	}
	if k, _ := a.DB.SkillByID(ctx, "briefing", 0); !k.Shared() || k.Version != 1 {
		t.Fatalf("a sprig's file changed a shared skill: %+v", k)
	}
	os.Remove(filepath.Join(skillDir, "briefing.md"))
	os.Remove(filepath.Join(skillDir, "pip-tea.md"))
	if probs := e.SyncSprigSkills(ctx); len(probs) != 0 {
		t.Fatal(probs)
	}
	if _, err := a.DB.SkillByID(ctx, "pip-tea", 0); err != store.ErrNotFound {
		t.Fatalf("removed file: %v", err)
	}

	// Taught in chat: saved for Pip only, written to its folder.
	e.HandleIncoming(ctx, dm("Pip, save own skill pip-tea"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "created: version 3") || !strings.Contains(r, "only Pip is offered it") {
		t.Fatalf("taught skill: %s", r)
	}
	b, err := os.ReadFile(filepath.Join(skillDir, "pip-tea.md"))
	if err != nil || !strings.Contains(string(b), "success_check: The user has a cup they like.") || !strings.Contains(string(b), "name: kind") {
		t.Fatalf("written file: %s %v", b, err)
	}
	if probs := e.SyncSprigSkills(ctx); len(probs) != 0 {
		t.Fatal(probs)
	}
	if k, _ := a.DB.SkillByID(ctx, "pip-tea", 0); k.Version != 3 || k.SprigID != "pip" {
		t.Fatalf("reloading its own file made a new version: %+v", k)
	}

	// Promoting it takes the owner or an admin, and goes in the trail.
	e.HandleIncoming(ctx, dm("Pip, promote skill pip-tea"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "in the shared library now") {
		t.Fatalf("promote: %s", r)
	}
	if k, _ := a.DB.SkillByID(ctx, "pip-tea", 0); !k.Shared() {
		t.Fatalf("not promoted: %+v", k)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "pip-tea.md.promoted")); err != nil {
		t.Fatal("promoted file not kept aside:", err)
	}
	if probs := e.SyncSprigSkills(ctx); len(probs) != 0 {
		t.Fatal(probs)
	}
	if k, _ := a.DB.SkillByID(ctx, "pip-tea", 0); !k.Shared() {
		t.Fatalf("a reload took the promoted skill back: %+v", k)
	}
	trail, _ := a.DB.Trail(ctx, 0, 20)
	found := false
	for _, tr := range trail {
		found = found || tr.Kind == store.TrailSkill && strings.Contains(tr.Summary, "pip-tea moved from Pip's own skills")
	}
	if !found {
		t.Fatalf("promotion not in the trail: %+v", trail)
	}
}

// A member teaches their own private sprig a skill; it isn't theirs to put
// in the shared library, and nobody else's sprig is offered it.
func TestMemberTeachesOwnSprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	onboard(t, a, fc, ctx)
	member, err := a.DB.CreateUser(ctx, "discord", "member", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "fern.md"), []byte(sprigFile("fern", "Fern", member.ID)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	say := func(text string) {
		in := dm(text)
		in.UserID, in.ChatID = "member", "dm-member"
		e.HandleIncoming(ctx, in)
		fc.take()
	}
	say("Fern, hello")
	req := lastChatReq(fm)
	offered := map[string]bool{}
	for _, d := range req.Tools {
		offered[d.Function.Name] = true
	}
	if !offered["save_skill"] || !offered["use_skill"] {
		t.Fatalf("a member's own sprig should be teachable: %v", offered)
	}
	say("Fern, save own skill fern-tea")
	if r := lastToolResult(fm); !strings.Contains(r, "created: version 1") {
		t.Fatalf("member teaches: %s", r)
	}
	if k, _ := a.DB.SkillByID(ctx, "fern-tea", 0); k == nil || k.SprigID != "fern" {
		t.Fatalf("owner: %+v", k)
	}
	say("Fern, promote skill fern-tea")
	if r := lastToolResult(fm); !strings.Contains(r, "only the owner or an admin") {
		t.Fatalf("member promoted: %s", r)
	}
	// Juni (a family sprig) isn't offered it, and the member can't save
	// to the shared library through her.
	say("Juniper, hello")
	if p := systemPromptOf(lastChatReq(fm)); strings.Contains(p, "fern-tea") {
		t.Fatal("Juni offered Fern's skill")
	}
}
