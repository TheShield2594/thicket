package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A reserved approval can't be answered until it is opened, and only a
// reserved one can be opened or dropped.
func TestReservedApprovals(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	a := Approval{UserID: u.ID, Character: "juniper", ChatRef: "discord:1", Tool: "remember", Args: "{}"}
	id, err := s.ReserveApproval(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LatestPendingApproval(ctx, u.ID, "discord:1", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reserved approval is answerable: %v", err)
	}
	if ok, _ := s.DecideApproval(ctx, id, "approved"); ok {
		t.Fatal("a reserved approval was decided")
	}
	if err := s.OpenApproval(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestPendingApproval(ctx, u.ID, "discord:1", time.Hour); err != nil || got.ID != id {
		t.Fatalf("opened approval: %+v %v", got, err)
	}
	if err := s.OpenApproval(ctx, id, ""); err == nil {
		t.Fatal("opened twice")
	}
	_ = s.DropApproval(ctx, id) // pending now: not dropped
	if _, err := s.ApprovalByID(ctx, id); err != nil {
		t.Fatalf("a pending approval was dropped: %v", err)
	}

	gone, _ := s.ReserveApproval(ctx, a)
	if err := s.DropApproval(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovalByID(ctx, gone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped approval still there: %v", err)
	}
}

// A prompt's message is settled once, by whoever takes it; the sweep
// finds those that ended with it still showing.
func TestApprovalPromptMsg(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	a := Approval{UserID: u.ID, Character: "juniper", ChatRef: "discord:1", Tool: "remember", Args: "{}"}
	id, _ := s.ReserveApproval(ctx, a)
	if got, _ := s.ApprovalByID(ctx, id); !got.Reserved {
		t.Fatal("not reserved")
	}
	if err := s.OpenApproval(ctx, id, "m1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ApprovalByID(ctx, id); got.Reserved {
		t.Fatal("still reserved once open")
	}
	if aps, _ := s.UnsettledApprovals(ctx, time.Hour); len(aps) != 0 {
		t.Fatalf("a waiting approval is unsettled: %+v", aps)
	}
	s.DecideApproval(ctx, id, "approved")
	if got, _ := s.ApprovalByID(ctx, id); got.Reserved {
		t.Fatal("decided reads as reserved")
	}
	if aps, _ := s.UnsettledApprovals(ctx, time.Hour); len(aps) != 1 || aps[0].ID != id || aps[0].Status != "approved" || aps[0].ChatRef != "discord:1" {
		t.Fatalf("unsettled: %+v", aps)
	}
	if msg, err := s.TakePromptMsg(ctx, id); err != nil || msg != "m1" {
		t.Fatalf("take: %q %v", msg, err)
	}
	if msg, _ := s.TakePromptMsg(ctx, id); msg != "" {
		t.Fatalf("taken twice: %q", msg)
	}
	if aps, _ := s.UnsettledApprovals(ctx, time.Hour); len(aps) != 0 {
		t.Fatalf("settled but listed: %+v", aps)
	}

	// Expired by age: the sweep expires it and lists it.
	old, _ := s.ReserveApproval(ctx, a)
	s.OpenApproval(ctx, old, "m2")
	s.db.ExecContext(ctx, "UPDATE approvals SET created_at = created_at - 7200 WHERE id = ?", old)
	if aps, _ := s.UnsettledApprovals(ctx, time.Hour); len(aps) != 1 || aps[0].ID != old || aps[0].Status != "expired" {
		t.Fatalf("expired: %+v", aps)
	}
	if msg, _ := s.TakePromptMsg(ctx, 999); msg != "" {
		t.Fatalf("no such approval: %q", msg)
	}
}
