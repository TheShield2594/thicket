package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

const newcomerHeading = "Welcome tour (they just joined)"

const ownWren = `---
id: wren
name: Wren
role: Keeps Sam's secret reading list.
belongs_to: %d
personality:
  traits: [bookish]
voice:
  style: [short]
---
Wren reads.
`

// Adults who join later get the short tour of what's already here, with
// the family sprigs named by Thicket (never anyone's private sprig), once.
func TestNewcomerTour(t *testing.T) {
	h := setupHousehold(t)
	a := h.a
	// The owner has a private sprig Kim must never hear about.
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "wren.md"), []byte(strings.Replace(ownWren, "%d", strconv.FormatInt(h.owner.ID, 10), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	if out := h.kimSays("hi, I'm new"); personas(out) != "Juni" {
		t.Fatalf("newcomer answered by %q", personas(out))
	}
	sys := lastChatPrompt(h.fm)
	for _, want := range []string{newcomerHeading, "The family sprigs:\n- Hal: Looks after the house.", "family memory", "Calendar →", "sprig of their own"} {
		if !strings.Contains(sys, want) {
			t.Errorf("tour missing %q", want)
		}
	}
	if strings.Contains(sys, onboardingHeading) || strings.Contains(sys, "secret reading list") {
		t.Fatalf("newcomer got the owner's setup or a private sprig:\n%s", sys)
	}
	// Once they've had it, it's gone.
	h.kimSays("that's the tour, thanks")
	h.kimSays("what's for dinner?")
	if sys := lastChatPrompt(h.fm); strings.Contains(sys, newcomerHeading) {
		t.Fatal("the tour came back after complete_onboarding")
	}
}

// An admin who joined through an invite gets the newcomer's tour, not the
// owner's setup.
func TestInvitedAdminGetsNewcomerTour(t *testing.T) {
	h := setupHousehold(t)
	if err := h.a.DB.SetUserRole(h.ctx, h.kim.ID, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.DB.DB().Exec("UPDATE users SET invited_by = ? WHERE id = ?", h.owner.ID, h.kim.ID); err != nil {
		t.Fatal(err)
	}
	h.kimSays("hello!")
	sys := lastChatPrompt(h.fm)
	if !strings.Contains(sys, newcomerHeading) || strings.Contains(sys, onboardingHeading) {
		t.Fatalf("invited admin's tour:\n%s", sys)
	}
	if !strings.Contains(sys, "They're an admin") {
		t.Error("an admin isn't told they can invite people and change settings")
	}
}

// A kid's first message gets a kid-sized hello from their kid sprig, once;
// Juni never greets them.
func TestKidFirstHello(t *testing.T) {
	h := setupHousehold(t)
	out := h.kidSays("hi")
	if personas(out) != "Bramble" {
		t.Fatalf("kid answered by %q", personas(out))
	}
	sys := lastChatPrompt(h.fm)
	if !strings.Contains(sys, "Saying hello (the first time you talk)") {
		t.Fatalf("no first hello:\n%s", sys)
	}
	if strings.Contains(sys, newcomerHeading) || strings.Contains(sys, onboardingHeading) {
		t.Fatal("a kid got an adult's tour")
	}
	if kid, _ := h.a.DB.UserByID(h.ctx, h.kid.ID); kid.OnboardedAt.IsZero() {
		t.Fatal("the hello wasn't recorded")
	}
	h.kidSays("let's play")
	if sys := lastChatPrompt(h.fm); strings.Contains(sys, "Saying hello") {
		t.Fatal("the kid was greeted twice")
	}
}

// A kid who joins through an invite is greeted by their kid sprig at once,
// and that counts as the hello.
func TestInvitedKidGreetedOnce(t *testing.T) {
	h := setupHousehold(t)
	if err := h.a.Engine.Welcome(h.ctx, h.kid, "discord:dm-max"); err != nil {
		t.Fatal(err)
	}
	out := h.fc.take()
	if personas(out) != "Bramble" {
		t.Fatalf("welcome by %q", personas(out))
	}
	h.kidSays("hi")
	if sys := lastChatPrompt(h.fm); strings.Contains(sys, "Saying hello") {
		t.Fatal("greeted again on the first message")
	}
}
