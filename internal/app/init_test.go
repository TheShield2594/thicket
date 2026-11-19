package app

import (
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
	firstrun "github.com/theshield2594/thicket/internal/setup"
)

// After `thicket init`, with nothing in the environment, the first
// message reaches Juni herself (not the "paste a key" script), the owner
// is who init said, and Juni is told not to ask for what's already set.
func TestInitThenFirstMessageReachesJuni(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	s := &firstrun.Setup{DB: a.DB, Secrets: a.Secrets, Settings: a.Settings} // offline: nothing to check against
	if _, err := s.OpenRouterKey(ctx, "sk-or-v1-"+strings.Repeat("ab12", 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Timezone(ctx, "America/Chicago"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Owner(ctx, "discord:4242"); err != nil {
		t.Fatal(err)
	}
	if !a.Engine.ModelReady(ctx) {
		t.Fatal("the stored key should make the model ready")
	}

	// Someone else gets there first: they can't claim the install.
	a.Engine.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "dm-x", MessageID: "1", UserID: "999", UserName: "Eve", Text: "hi", IsDM: true})
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "private") {
		t.Fatalf("a stranger before the owner: %+v", out)
	}

	fm.mu.Lock()
	fm.say = "Hello! I'm Juni. What should I call you?"
	fm.mu.Unlock()
	a.Engine.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "dm-owner", MessageID: "2", UserID: "4242", UserName: "Sam", Text: "hello?", IsDM: true})
	out := fc.take()
	if len(out) != 1 || out[0].persona != "Juni" || !strings.Contains(out[0].text, "I'm Juni") || strings.Contains(out[0].text, "openrouter.ai/keys") {
		t.Fatalf("first message after init: %+v", out)
	}
	owner, err := a.DB.UserByExternal(ctx, "discord", "4242")
	if err != nil || owner.Role != "owner" || owner.Timezone != "America/Chicago" {
		t.Fatalf("owner: %+v %v", owner, err)
	}
	fm.mu.Lock()
	sys := ""
	for _, r := range fm.reqs {
		if strings.Contains(r.Messages[0].Content, "First-run onboarding") {
			sys = r.Messages[0].Content
		}
	}
	fm.mu.Unlock()
	if !strings.Contains(sys, "First-run onboarding") || !strings.Contains(sys, "Their timezone is already set (America/Chicago)") {
		t.Fatalf("Juni's onboarding should skip the timezone question; system prompt:\n%s", sys)
	}
}
