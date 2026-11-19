package store

import (
	"context"
	"fmt"
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

// The texts of proactive messages are kept, newest first, only the last
// few per sprig and person; LastProactiveAt sees them and the daily counts.
func TestRememberSaid(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	if at, err := s.LastProactiveAt(ctx, u.ID, "pip"); err != nil || !at.IsZero() {
		t.Fatalf("never wrote: %v %v", at, err)
	}
	start := time.Unix(1_800_000_000, 0)
	for i := 0; i < SaidKeep+2; i++ {
		if err := s.RememberSaid(ctx, u.ID, "pip", fmt.Sprintf("message %d", i), start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RememberSaid(ctx, u.ID, "juniper", "another sprig", start); err != nil {
		t.Fatal(err)
	}
	said, err := s.RecentSaid(ctx, u.ID, "pip", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != SaidKeep || said[0].Text != fmt.Sprintf("message %d", SaidKeep+1) || said[len(said)-1].Text != "message 2" {
		t.Fatalf("kept: %+v", said)
	}
	if other, _ := s.RecentSaid(ctx, u.ID, "juniper", 10); len(other) != 1 {
		t.Fatalf("another sprig's messages: %+v", other)
	}
	if at, _ := s.LastProactiveAt(ctx, u.ID, "pip"); !at.Equal(start.Add(time.Duration(SaidKeep+1) * time.Hour)) {
		t.Fatalf("last wrote %v", at)
	}
	// A message counted for the budget but not kept (from before texts
	// were kept) still marks when the sprig last wrote.
	later := start.Add(48 * time.Hour)
	if err := s.RecordProactive(ctx, u.ID, "pip", later); err != nil {
		t.Fatal(err)
	}
	if at, _ := s.LastProactiveAt(ctx, u.ID, "pip"); !at.Equal(later) {
		t.Fatalf("last wrote %v, want %v", at, later)
	}
}
