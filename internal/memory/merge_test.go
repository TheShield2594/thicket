package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func existing() []store.Memory {
	return []store.Memory{
		{ID: 1, UserID: 7, Kind: "fact", Content: "Has a dog named Biscuit.", Source: "auto", UpdatedAt: now},
		{ID: 2, UserID: 7, Kind: "preference", Content: "Prefers tea over coffee.", Source: "user", Locked: true, UpdatedAt: now},
		{ID: 3, UserID: 7, Character: "juniper", Kind: "inside_joke", Content: "Calls the office plant Gerald.", Source: "auto", UpdatedAt: now},
	}
}

func plan(ops ...Op) Plan {
	return BuildPlan(PlanInput{UserID: 7, Scopes: map[string]bool{"juniper": true}, Existing: existing(), Now: now, Loc: time.UTC}, ops)
}

func TestAddNewMemory(t *testing.T) {
	p := plan(Op{Op: "add", Scope: "shared", Kind: "person", Content: "Sister Maya lives in Leeds."})
	if len(p.Changes) != 1 || p.Changes[0].Action != "add" {
		t.Fatalf("want one add, got %+v (skipped %v)", p.Changes, p.Skipped)
	}
	m := p.Changes[0].Memory
	if m.UserID != 7 || m.Character != "" || m.Kind != "person" || m.Source != "auto" || m.Locked {
		t.Fatalf("unexpected memory %+v", m)
	}
}

func TestNearDuplicateBecomesUpdate(t *testing.T) {
	p := plan(Op{Op: "add", Scope: "shared", Kind: "fact", Content: "Has a dog named Biscuit, a beagle."})
	if len(p.Changes) != 1 || p.Changes[0].Action != "update" || p.Changes[0].Memory.ID != 1 {
		t.Fatalf("want update of #1, got %+v (skipped %v)", p.Changes, p.Skipped)
	}
}

func TestExactDuplicateIsTouch(t *testing.T) {
	p := plan(Op{Op: "add", Scope: "shared", Content: "has a dog named biscuit"})
	if len(p.Changes) != 1 || p.Changes[0].Action != "touch" {
		t.Fatalf("want touch, got %+v", p.Changes)
	}
}

func TestLockedMemoriesAreNeverChanged(t *testing.T) {
	p := plan(
		Op{Op: "update", ID: 2, Content: "Prefers coffee now."},
		Op{Op: "expire", ID: 2},
		Op{Op: "add", Scope: "shared", Content: "Prefers tea over coffee, always."},
	)
	if len(p.Changes) != 0 {
		t.Fatalf("locked memory was changed: %+v", p.Changes)
	}
	if len(p.Skipped) != 3 {
		t.Fatalf("want 3 skipped, got %v", p.Skipped)
	}
}

func TestUnknownIDsRejected(t *testing.T) {
	p := plan(Op{Op: "update", ID: 99, Content: "x y z"}, Op{Op: "expire", ID: 98})
	if len(p.Changes) != 0 || len(p.Skipped) != 2 {
		t.Fatalf("got changes %+v skipped %v", p.Changes, p.Skipped)
	}
}

func TestScopeMustBeParticipant(t *testing.T) {
	p := plan(Op{Op: "add", Scope: "pip", Content: "Pip owes the user a story."})
	if len(p.Changes) != 0 {
		t.Fatalf("non-participant scope accepted: %+v", p.Changes)
	}
	p = plan(Op{Op: "add", Scope: "juniper", Kind: "inside_joke", Content: "Laughs about the watering can incident."})
	if len(p.Changes) != 1 || p.Changes[0].Memory.Character != "juniper" {
		t.Fatalf("participant scope rejected: %+v %v", p.Changes, p.Skipped)
	}
}

func TestDuplicateScopedSeparately(t *testing.T) {
	// Same words in a different scope is not a duplicate.
	p := plan(Op{Op: "add", Scope: "shared", Kind: "fact", Content: "Calls the office plant Gerald."})
	if len(p.Changes) != 1 || p.Changes[0].Action != "add" {
		t.Fatalf("got %+v", p.Changes)
	}
}

func TestRejectsSecretsAndBadContent(t *testing.T) {
	p := plan(
		Op{Op: "add", Scope: "shared", Content: "API key is sk-or-v1-0123456789abcdef0123456789abcdef"},
		Op{Op: "add", Scope: "shared", Content: ""},
		Op{Op: "add", Scope: "shared", Content: strings.Repeat("long ", 100)},
		Op{Op: "add", Scope: "shared", Kind: "gossip", Content: "Something odd."},
		Op{Op: "delete-everything"},
	)
	if len(p.Changes) != 0 || len(p.Skipped) != 5 {
		t.Fatalf("got changes %+v skipped %v", p.Changes, p.Skipped)
	}
}

func TestFollowupNeedsValidFutureDate(t *testing.T) {
	p := plan(
		Op{Op: "add", Scope: "juniper", Kind: "followup", Content: "Ask how the interview went.", Due: "2026-11-18"},
		Op{Op: "add", Scope: "juniper", Kind: "followup", Content: "Ask about the trip.", Due: "2020-01-01"},
		Op{Op: "add", Scope: "juniper", Kind: "followup", Content: "Ask about the move.", Due: "soon"},
	)
	if len(p.Changes) != 1 {
		t.Fatalf("want 1 followup, got %+v skipped %v", p.Changes, p.Skipped)
	}
	want := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if !p.Changes[0].Memory.DueAt.Equal(want) {
		t.Fatalf("due = %v, want %v", p.Changes[0].Memory.DueAt, want)
	}
}

func TestContextExpires(t *testing.T) {
	p := plan(Op{Op: "add", Scope: "shared", Kind: "context", Content: "Is tired today after a long shift."})
	if len(p.Changes) != 1 || !p.Changes[0].Memory.ExpiresAt.Equal(now.Add(ContextTTL)) {
		t.Fatalf("context memory should expire: %+v", p.Changes)
	}
}

func TestInBatchDuplicates(t *testing.T) {
	p := plan(
		Op{Op: "add", Scope: "shared", Content: "Plays the cello on weekends."},
		Op{Op: "add", Scope: "shared", Content: "Plays the cello on the weekends."},
	)
	if len(p.Changes) != 1 {
		t.Fatalf("want 1, got %+v", p.Changes)
	}
}

func TestOpCap(t *testing.T) {
	var ops []Op
	for i := 0; i < MaxOpsPerRun+5; i++ {
		ops = append(ops, Op{Op: "add", Scope: "shared", Content: "Distinct fact number " + strings.Repeat("x", i+1)})
	}
	p := plan(ops...)
	if len(p.Changes) > MaxOpsPerRun {
		t.Fatalf("cap not enforced: %d", len(p.Changes))
	}
}

func TestUnchangedUpdateIsNoop(t *testing.T) {
	p := plan(Op{Op: "update", ID: 1, Content: "Has a dog named Biscuit."})
	if len(p.Changes) != 0 {
		t.Fatalf("no-op update produced %+v", p.Changes)
	}
}

func TestSelectPrefersRelevant(t *testing.T) {
	mems := []store.Memory{
		{ID: 1, Kind: "fact", Content: "Has a dog named Biscuit.", UpdatedAt: now.AddDate(0, -2, 0)},
		{ID: 2, Kind: "project", Content: "Renovating the kitchen; tiles arrive in December.", UpdatedAt: now.AddDate(0, -2, 0)},
		{ID: 3, Kind: "context", Content: "Had pasta for lunch.", UpdatedAt: now.AddDate(0, -2, 0)},
	}
	got := Select(mems, "when do the kitchen tiles come?", now, 2, 1000)
	if len(got) != 2 || got[0].ID != 2 {
		t.Fatalf("want kitchen first, got %+v", got)
	}
	if got := Select(mems, "", now, 10, 30); len(got) != 1 {
		t.Fatalf("char budget not respected: %d", len(got))
	}
}

func TestParseOps(t *testing.T) {
	ops, err := ParseOps("```json\n{\"ops\": [{\"op\": \"expire\", \"id\": 3}]}\n```")
	if err != nil || len(ops) != 1 || ops[0].ID != 3 {
		t.Fatalf("got %+v, %v", ops, err)
	}
	if _, err := ParseOps("I think the user likes tea"); err == nil {
		t.Fatal("expected error for non-JSON")
	}
}

func TestFollowupDueChangeIsKept(t *testing.T) {
	ex := append(existing(), store.Memory{ID: 4, UserID: 7, Character: "juniper", Kind: "followup", Content: "Ask how the interview went.",
		DueAt: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), Source: "auto", UpdatedAt: now})
	p := BuildPlan(PlanInput{UserID: 7, Scopes: map[string]bool{"juniper": true}, Existing: ex, Now: now, Loc: time.UTC},
		[]Op{{Op: "add", Scope: "juniper", Kind: "followup", Content: "Ask how the interview went.", Due: "2026-11-20"}})
	if len(p.Changes) != 1 || p.Changes[0].Action != "update" || p.Changes[0].Memory.DueAt.Day() != 20 {
		t.Fatalf("new due date lost: %+v", p.Changes)
	}
}

func TestUpdateToFollowupNeedsDue(t *testing.T) {
	p := plan(Op{Op: "update", ID: 1, Kind: "followup", Content: "Ask about Biscuit's vet visit."})
	if len(p.Changes) != 0 {
		t.Fatalf("followup without due accepted: %+v", p.Changes)
	}
	p = plan(Op{Op: "update", ID: 1, Content: "Has a dog named Biscuit.", Due: "whenever"})
	if len(p.Changes) != 0 || len(p.Skipped) != 1 {
		t.Fatalf("bad due silently ignored: %+v %v", p.Changes, p.Skipped)
	}
}

func TestSingleLetterDifferencesMatter(t *testing.T) {
	ex := []store.Memory{{ID: 9, UserID: 7, Kind: "fact", Content: "Has blood type A.", Source: "auto", UpdatedAt: now}}
	p := BuildPlan(PlanInput{UserID: 7, Existing: ex, Now: now}, []Op{{Op: "update", ID: 9, Content: "Has blood type B."}})
	if len(p.Changes) != 1 || p.Changes[0].Memory.Content != "Has blood type B." {
		t.Fatalf("correction dropped: %+v", p.Changes)
	}
}

func TestLimitCountsCharactersNotBytes(t *testing.T) {
	content := strings.Repeat("猫", 150) // 150 characters, 450 bytes
	p := plan(Op{Op: "add", Scope: "shared", Content: content})
	if len(p.Changes) != 1 {
		t.Fatalf("150-character memory rejected: %v", p.Skipped)
	}
}

func TestKindChangeMovesExpiry(t *testing.T) {
	ex := []store.Memory{
		{ID: 1, UserID: 7, Kind: "fact", Content: "Is visiting Lisbon.", Source: "auto", UpdatedAt: now},
		{ID: 2, UserID: 7, Kind: "context", Content: "Learning to sail.", Source: "auto", UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	p := BuildPlan(PlanInput{UserID: 7, Existing: ex, Now: now}, []Op{
		{Op: "update", ID: 1, Kind: "context", Content: "Is visiting Lisbon this week."},
		{Op: "update", ID: 2, Kind: "project", Content: "Learning to sail; lessons every Sunday."},
	})
	if len(p.Changes) != 2 {
		t.Fatalf("got %+v %v", p.Changes, p.Skipped)
	}
	if c := p.Changes[0]; !c.SetExpiry || !c.Memory.ExpiresAt.Equal(now.Add(ContextTTL)) {
		t.Fatalf("fact->context should gain an expiry: %+v", c)
	}
	if c := p.Changes[1]; !c.SetExpiry || !c.Memory.ExpiresAt.IsZero() {
		t.Fatalf("context->project should lose its expiry: %+v", c)
	}
}
