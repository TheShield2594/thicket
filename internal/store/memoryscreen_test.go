package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Changes made on the memory screen are recorded with who made them and
// undone one by one, newest first; review changes are not theirs to undo.
func TestUndoMemoryScreenChanges(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	ids, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", By: u.ID,
		Memory: Memory{UserID: u.ID, Character: "juniper", Kind: "tone", Content: "Prefers short answers, no emoji.", Source: "user", Locked: true, Origin: "person:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	if m, _ := s.MemoryByID(ctx, id); m.Origin != "person:1" || !m.Locked {
		t.Fatalf("added: %+v", m)
	}
	// An edit that unlocks, then a lock: two events, undone in turn.
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "update", By: u.ID, SetLock: true,
		Memory: Memory{ID: id, Content: "Prefers long, detailed answers.", Kind: "tone", Locked: false}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "lock", By: u.ID, Memory: Memory{ID: id, Locked: true}}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MemoryByID(ctx, id); m.Content != "Prefers long, detailed answers." || !m.Locked {
		t.Fatalf("edited: %+v", m)
	}
	if undoable, _ := s.UndoableMemories(ctx, []int64{id}); !undoable[id] {
		t.Fatal("edit not undoable")
	}
	m, err := s.UndoMemoryChange(ctx, id, u.ID) // the lock
	if err != nil || m.Locked || m.Content != "Prefers long, detailed answers." {
		t.Fatalf("undo lock: %+v %v", m, err)
	}
	m, err = s.UndoMemoryChange(ctx, id, u.ID) // the edit
	if err != nil || !m.Locked || m.Content != "Prefers short answers, no emoji." {
		t.Fatalf("undo edit: %+v %v", m, err)
	}
	m, err = s.UndoMemoryChange(ctx, id, u.ID) // the add
	if err != nil || m != nil {
		t.Fatalf("undo add: %+v %v", m, err)
	}
	if _, err := s.MemoryByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undone add still there: %v", err)
	}

	// Undoing an unlock keeps what the review wrote after it, and an
	// undo that would throw a later change away is refused.
	ids, _ = s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", By: u.ID,
		Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Has a dog.", Source: "user", Locked: true, Origin: "person:1"}}})
	id = ids[0]
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "lock", By: u.ID, Memory: Memory{ID: id, Locked: false}}}); err != nil {
		t.Fatal(err)
	}
	review, _ := s.CreateExtractionRun(ctx, 0, u.ID, "m")
	if _, err := s.ApplyMemoryChanges(ctx, review, []MemoryChange{{Action: "update", Memory: Memory{ID: id, Content: "Has a dog named Biscuit."}}}); err != nil {
		t.Fatal(err)
	}
	m, err = s.UndoMemoryChange(ctx, id, u.ID) // the unlock
	if err != nil || !m.Locked || m.Content != "Has a dog named Biscuit." || m.Kind != "fact" {
		t.Fatalf("undo unlock: %+v %v", m, err)
	}
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "update", By: u.ID, SetLock: true,
		Memory: Memory{ID: id, Content: "Has two dogs.", Locked: true}}}); err != nil {
		t.Fatal(err)
	}
	// A sprig (no person, no run) rewrites it after the edit.
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "update", Memory: Memory{ID: id, Content: "Has three dogs."}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoMemoryChange(ctx, id, u.ID); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("undo over a later change: %v", err)
	}
	if m, _ := s.MemoryByID(ctx, id); m.Content != "Has three dogs." {
		t.Fatalf("refused undo changed the memory: %+v", m)
	}

	// An update that leaves content out keeps the content as it is now,
	// not as the editor last saw it.
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "update", By: u.ID, SetLock: true,
		Memory: Memory{ID: id, Kind: "preference", Locked: true}}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MemoryByID(ctx, id); m.Content != "Has three dogs." || m.Kind != "preference" {
		t.Fatalf("kind-only edit: %+v", m)
	}

	// A review's changes are rolled back by run, not undone here.
	run, _ := s.CreateExtractionRun(ctx, 0, u.ID, "m")
	ids, _ = s.ApplyMemoryChanges(ctx, run, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Likes tea.", Source: "auto", Origin: OriginReview}}})
	if _, err := s.UndoMemoryChange(ctx, ids[0], u.ID); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undid a review change: %v", err)
	}
	if undoable, _ := s.UndoableMemories(ctx, ids); undoable[ids[0]] {
		t.Fatal("a review change shows as undoable")
	}
}

// A kid sprig's parents see what it remembers about its kids: their
// shared layer and its own space, never another sprig's or family memory.
func TestKidSprigMemories(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	kid, _ := s.CreateUser(ctx, "discord", "k", "Max", RoleKid)
	other, _ := s.CreateUser(ctx, "discord", "o", "Kim", RoleMember)
	add := func(user int64, char, content string, household bool) {
		t.Helper()
		if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: user, Character: char, Kind: "fact", Content: content, Source: "auto", Household: household}}}); err != nil {
			t.Fatal(err)
		}
	}
	add(kid.ID, "bramble", "Loves dinosaurs.", false)
	add(kid.ID, "", "Is nine.", false)
	add(kid.ID, "pebble", "Pebble's own.", false)
	add(kid.ID, "", "Family note.", true)
	add(other.ID, "bramble", "Not the kid's.", false)
	mems, err := s.KidSprigMemories(ctx, []int64{kid.ID}, "bramble")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mems {
		got = append(got, m.Content)
	}
	if strings.Join(got, "|") != "Loves dinosaurs.|Is nine." {
		t.Fatalf("kid sprig memories: %v", got)
	}
}
