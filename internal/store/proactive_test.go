package store

import (
	"context"
	"testing"
	"time"
)

// A held message stays stored while it's being delivered: one flush
// claims it at a time, a failed delivery releases it, a claim left by a
// crash goes stale and can be taken over, and only FinishHeld removes it.
func TestHeldMessageClaims(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	now := time.Unix(1_800_000_000, 0)
	id, err := s.HoldMessage(ctx, HeldMessage{UserID: u.ID, Character: "juniper", Reason: HeldQuiet, Text: "hi", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(at time.Time) bool {
		t.Helper()
		ok, err := s.ClaimHeld(ctx, id, at, at.Add(-10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !claim(now) {
		t.Fatal("first claim refused")
	}
	if claim(now.Add(time.Minute)) {
		t.Fatal("a second flush claimed a message already being delivered")
	}
	if held, _ := s.HeldMessages(ctx, u.ID, ""); len(held) != 1 {
		t.Fatalf("a claimed message must stay stored, have %d", len(held))
	}
	if err := s.ReleaseHeld(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !claim(now.Add(2 * time.Minute)) {
		t.Fatal("a released message couldn't be claimed again")
	}
	// The claimer crashed: after the timeout another flush takes over.
	if !claim(now.Add(2*time.Minute + 11*time.Minute)) {
		t.Fatal("a stale claim wasn't taken over")
	}
	if err := s.FinishHeld(ctx, id); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.HeldMessages(ctx, u.ID, ""); len(held) != 0 {
		t.Fatalf("finished message still held: %d", len(held))
	}
}
