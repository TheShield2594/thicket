package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

func hostPrompt(u *store.User, mod func(*promptInput)) string {
	juni := &character.Character{ID: "juniper", Name: "Juniper", Nickname: "Juni", Role: "The host.", Host: true,
		Personality: character.Personality{Traits: []string{"warm"}}, Voice: character.Voice{Style: []string{"brief"}}}
	in := promptInput{char: juni, user: u, roster: []*character.Character{juni}, now: time.Now(), isDM: true, hostTools: true}
	if mod != nil {
		mod(&in)
	}
	return buildSystemPrompt(in)
}

func TestOnboardingCoversHouseholdCalendarSandboxDesktop(t *testing.T) {
	owner := &store.User{ID: 1, Role: store.RoleOwner}
	sys := hostPrompt(owner, nil)
	for _, want := range []string{
		"just for you, or for your household?", // #34
		"invite_person", "kid sprig", "family memory",
		`Calendar → "Connect a calendar"`, "Never connect calendars in chat", `add "calendar"`, // #36
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("owner's onboarding missing %q", want)
		}
	}
	// #37: the sandbox only when it's connected; the desktop app only in the web app.
	if strings.Contains(sys, "sandbox") || strings.Contains(sys, "desktop app") {
		t.Error("sandbox or desktop app mentioned without reason")
	}
	sys = hostPrompt(owner, func(in *promptInput) { in.sandbox, in.sandboxIdle, in.web = true, true, true })
	for _, want := range []string{"separate Linux machine with no access to Thicket's data", "asks for their OK first", "mcp:sandbox/*", "docs/desktop.md"} {
		if !strings.Contains(sys, want) {
			t.Errorf("onboarding with the sandbox, in the web app, missing %q", want)
		}
	}
	// After setup: adding people any time, and the sandbox offer while no sprig has it.
	owner.OnboardedAt = time.Now()
	sys = hostPrompt(owner, func(in *promptInput) { in.onboarded, in.sandbox, in.sandboxIdle = true, true, true })
	if strings.Contains(sys, "First-run onboarding") || !strings.Contains(sys, "## Adding people") || !strings.Contains(sys, "## The sandbox computer") {
		t.Errorf("after onboarding:\n%s", sys)
	}
	sys = hostPrompt(owner, func(in *promptInput) { in.onboarded, in.sandbox = true, true })
	if strings.Contains(sys, "## The sandbox computer") {
		t.Error("the sandbox is offered though a sprig already has it")
	}
}

func TestNewcomerTour(t *testing.T) {
	member := &store.User{ID: 2, Role: store.RoleMember}
	sys := hostPrompt(member, nil)
	if !strings.Contains(sys, "Welcome tour") || strings.Contains(sys, "First-run onboarding") || strings.Contains(sys, "## Adding people") {
		t.Fatalf("a new member gets the short tour, not the owner's:\n%s", sys)
	}
	if strings.Contains(sys, "invited to make a sprig of their own") {
		t.Error("own-sprig step without the invite asking for it")
	}
	member.OwnSprig = true
	if sys = hostPrompt(member, nil); !strings.Contains(sys, "private: true") {
		t.Error("own-sprig step missing")
	}
	member.OnboardedAt = time.Now()
	if sys = hostPrompt(member, func(in *promptInput) { in.onboarded = true }); strings.Contains(sys, "Welcome tour") {
		t.Error("the tour stays after it's done")
	}
	kid := &store.User{ID: 3, Role: store.RoleKid}
	if sys = hostPrompt(kid, nil); strings.Contains(sys, "Welcome tour") {
		t.Error("kids never get the adults' tour")
	}
}
