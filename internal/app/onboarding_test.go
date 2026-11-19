package app

// The guided part of first-run onboarding: the conversation with Juni
// after the scripted key step. The guide reaches the host's prompt only
// until complete_onboarding, its steps lead to the right tools, and it is
// the owner's alone.

import (
	"strings"
	"testing"
)

const onboardingHeading = "First-run onboarding"

// lastChatPrompt is the system prompt of the newest chat request.
func lastChatPrompt(fm *fakeModel) string {
	req := lastChatReq(fm)
	if len(req.Messages) == 0 {
		return ""
	}
	return req.Messages[0].Content
}

func TestOnboardingGuideUntilComplete(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	say := func(text string) []sent {
		a.Engine.HandleIncoming(ctx, dm(text))
		return fc.take()
	}

	// Before complete_onboarding: Juni has the guide and its tools.
	if out := say("hello!"); len(out) != 1 || out[0].persona != "Juni" {
		t.Fatalf("first chat: %+v", out)
	}
	sys := lastChatPrompt(fm)
	if !strings.HasPrefix(sys, "You are Juni") || !strings.Contains(sys, onboardingHeading) {
		t.Fatalf("Juni's prompt should carry the onboarding guide:\n%s", sys)
	}
	if !strings.Contains(sys, "(you don't know their name yet)") || strings.Contains(sys, "Their timezone is already set") {
		t.Fatalf("nothing about the owner is known yet:\n%s", sys)
	}
	tools := toolNames(fm)
	for _, must := range []string{"set_user_profile", "complete_onboarding"} {
		if !tools[must] {
			t.Errorf("onboarding tool %s not offered: %v", must, tools)
		}
	}

	// The name and timezone step: set_user_profile stores both.
	say("profile: Sam Rivers | Europe/London")
	owner, err := a.DB.UserByExternal(ctx, "discord", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Name != "Sam Rivers" || owner.Timezone != "Europe/London" {
		t.Fatalf("profile not saved: name %q timezone %q", owner.Name, owner.Timezone)
	}
	if !owner.OnboardedAt.IsZero() {
		t.Fatal("setting a profile shouldn't finish onboarding")
	}
	// A timezone that isn't one is refused, and nothing changes.
	say("profile: Sammy | Mars/Olympus_Mons")
	owner, _ = a.DB.UserByExternal(ctx, "discord", "owner")
	if owner.Name != "Sam Rivers" || owner.Timezone != "Europe/London" {
		t.Fatalf("a bad timezone changed the profile: name %q timezone %q", owner.Name, owner.Timezone)
	}

	// Still onboarding: the guide stays, now knowing who they are.
	say("what's next?")
	sys = lastChatPrompt(fm)
	if !strings.Contains(sys, onboardingHeading) {
		t.Fatal("the guide left before complete_onboarding")
	}
	if !strings.Contains(sys, "Name: Sam Rivers.") || !strings.Contains(sys, "(Europe/London)") ||
		!strings.Contains(sys, "Their timezone is already set (Europe/London)") {
		t.Fatalf("the prompt should know the saved profile:\n%s", sys)
	}

	// The last step: complete_onboarding marks them onboarded...
	say("thanks, that's the tour done")
	owner, _ = a.DB.UserByExternal(ctx, "discord", "owner")
	if owner.OnboardedAt.IsZero() {
		t.Fatal("complete_onboarding didn't mark the owner onboarded")
	}

	// ...and the guide is gone from then on, the profile kept.
	say("morning!")
	sys = lastChatPrompt(fm)
	if !strings.HasPrefix(sys, "You are Juni") {
		t.Fatalf("expected Juni's prompt, got:\n%s", sys)
	}
	if strings.Contains(sys, onboardingHeading) || strings.Contains(sys, "Their timezone is already set") {
		t.Fatalf("the onboarding guide outlived complete_onboarding:\n%s", sys)
	}
	if !strings.Contains(sys, "Name: Sam Rivers.") {
		t.Fatalf("profile lost after onboarding:\n%s", sys)
	}
}

func TestOwnerOnboardingNotForMembersOrKids(t *testing.T) {
	h := setupHousehold(t)

	// An adult member who just joined talks to Juni, but not through the
	// owner's setup tour.
	if out := h.kimSays("hi Juni, I'm new here"); personas(out) != "Juni" {
		t.Fatalf("member answered by %q", personas(out))
	}
	if kim, _ := h.a.DB.UserByID(h.ctx, h.kim.ID); !kim.OnboardedAt.IsZero() {
		t.Fatal("test setup: the member should not be onboarded yet")
	}
	if sys := lastChatPrompt(h.fm); !strings.HasPrefix(sys, "You are Juni") || strings.Contains(sys, onboardingHeading) {
		t.Fatalf("a member got the owner's onboarding:\n%s", sys)
	}
	// Their own name and timezone are still theirs to set.
	if tools := toolNames(h.fm); !tools["set_user_profile"] {
		t.Error("a member can't set their own profile")
	}

	// A kid lands on their kid sprig, which has no onboarding and no host tools.
	if out := h.kidSays("hi"); personas(out) != "Bramble" {
		t.Fatalf("kid answered by %q", personas(out))
	}
	sys := lastChatPrompt(h.fm)
	if !strings.HasPrefix(sys, "You are Bramble") || strings.Contains(sys, onboardingHeading) {
		t.Fatalf("a kid got the owner's onboarding:\n%s", sys)
	}
	tools := toolNames(h.fm)
	for _, never := range []string{"set_user_profile", "complete_onboarding", "connect_chat_app", "add_integration", "invite_user"} {
		if tools[never] {
			t.Errorf("kid sprig offered onboarding tool %s", never)
		}
	}
	// Even naming Juni, a kid never reaches the host's onboarding.
	if out := h.kidSays("Juni, can you set me up?"); personas(out) != "Bramble" {
		t.Fatalf("kid reached %q", personas(out))
	}
	if sys := lastChatPrompt(h.fm); strings.Contains(sys, onboardingHeading) {
		t.Fatal("a kid got the owner's onboarding by asking for Juni")
	}
}
