package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// The review proposes relationship memory apart from facts, and it always
// belongs to one sprig.
func TestRelationshipOps(t *testing.T) {
	ops, err := ParseOps(`{"ops": [{"op": "add", "scope": "shared", "kind": "fact", "content": "Works nights."}],
		"relationship": [
			{"op": "add", "scope": "juniper", "kind": "tone", "content": "Prefers short answers, no emoji."},
			{"op": "add", "scope": "shared", "kind": "dislikes", "content": "Got annoyed at being asked twice."},
			{"op": "add", "scope": "juniper", "kind": "fact", "content": "Has a cat called Mog."}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 4 || ops[0].Rel || !ops[1].Rel {
		t.Fatalf("ops: %+v", ops)
	}
	p := plan(ops...)
	var got []string
	for _, ch := range p.Changes {
		got = append(got, ch.Memory.Kind+"@"+ch.Memory.Character)
	}
	// The shared dislike goes to the only sprig in the conversation; a
	// fact under "relationship" is refused.
	if strings.Join(got, ",") != "fact@,tone@juniper,dislikes@juniper" {
		t.Fatalf("changes: %v (skipped %v)", got, p.Skipped)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0], "not relationship memory") {
		t.Fatalf("skipped: %v", p.Skipped)
	}

	// With several sprigs, relationship memory must say whose it is.
	p = BuildPlan(PlanInput{UserID: 7, Scopes: map[string]bool{"juniper": true, "pip": true}, Now: now, Loc: time.UTC},
		[]Op{{Op: "add", Scope: "shared", Kind: "likes", Content: "Loves a good pun.", Rel: true}})
	if len(p.Changes) != 0 || len(p.Skipped) != 1 {
		t.Fatalf("shared relationship memory accepted: %+v %v", p.Changes, p.Skipped)
	}

	// A shared fact can't be turned into relationship memory.
	p = plan(Op{Op: "update", ID: 1, Kind: "likes", Content: "Has a dog named Biscuit."})
	if len(p.Changes) != 0 {
		t.Fatalf("shared memory became relationship memory: %+v", p.Changes)
	}

	// Relationship ops reach relationship memory only: never a fact.
	p = plan(Op{Op: "update", ID: 1, Content: "Prefers short answers, no emoji.", Rel: true},
		Op{Op: "expire", ID: 1, Rel: true})
	if len(p.Changes) != 0 || len(p.Skipped) != 2 {
		t.Fatalf("relationship op changed a fact: %+v %v", p.Changes, p.Skipped)
	}
	p = plan(Op{Op: "update", ID: 3, Content: "Calls the office plant Gerald the Great.", Rel: true})
	if len(p.Changes) != 1 || p.Changes[0].Action != "update" {
		t.Fatalf("relationship update refused: %+v %v", p.Changes, p.Skipped)
	}
	p = plan(Op{Op: "expire", ID: 3, Rel: true})
	if len(p.Changes) != 1 || p.Changes[0].Action != "expire" {
		t.Fatalf("relationship expire refused: %+v %v", p.Changes, p.Skipped)
	}
}

func TestSplitAndGroups(t *testing.T) {
	facts, rel := Split([]store.Memory{{Kind: "fact"}, {Character: "juniper", Kind: "tone"}, {Character: "juniper", Kind: "inside_joke"}, {Kind: "followup"},
		{Kind: "inside_joke"}, {Character: "juniper", Kind: "shared_history", Household: true}})
	if len(facts) != 4 || len(rel) != 2 {
		t.Fatalf("split: %d facts, %d relationship", len(facts), len(rel))
	}
	if GroupOf("dislikes") != GroupRelationship || GroupOf("person") != GroupFacts {
		t.Fatal("groups")
	}
	// Tone outranks an ordinary fact when nothing in the query matches.
	got := Select([]store.Memory{{ID: 1, Kind: "fact", Content: "Has a dog."}, {ID: 2, Kind: "tone", Content: "No emoji."}}, "hello", now, 1, 1000)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("select: %+v", got)
	}
}
