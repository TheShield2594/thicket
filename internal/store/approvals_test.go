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
	if err := s.OpenApproval(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestPendingApproval(ctx, u.ID, "discord:1", time.Hour); err != nil || got.ID != id {
		t.Fatalf("opened approval: %+v %v", got, err)
	}
	if err := s.OpenApproval(ctx, id); err == nil {
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
