package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/store"
)

// Two people in one household: neither sees the other's memories,
// private sprigs or inbox, and a member's attempt to raise a level on a
// sprig that isn't theirs is refused and logged.
func TestTwoPeopleStayApart(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	writeSolo(t, a, "ledger", h.owner.ID) // the owner's private sprig
	writeSolo(t, a, "wren", h.kim.ID)     // Kim's

	// Memories.
	h.ownerSay("remember my dog")
	mine, _ := a.DB.AllMemories(ctx, h.owner.ID)
	if len(mine) == 0 {
		t.Fatal("the owner's memory wasn't saved")
	}
	h.kimSays("hello juni")
	for _, m := range mustMemories(t, a, h.kim.ID) {
		if strings.Contains(m.Content, "Biscuit") {
			t.Fatalf("Kim has the owner's memory: %+v", m)
		}
	}
	if sys := lastChatReq(h.fm).Messages[0].Content; strings.Contains(sys, "Biscuit") {
		t.Fatal("the owner's memory reached a prompt for Kim")
	}

	// Private sprigs: not on the other's roster, and naming one doesn't
	// reach it.
	for _, c := range a.Chars.For(h.kim) {
		if c.ID == "ledger" {
			t.Fatal("the owner's private sprig is on Kim's roster")
		}
	}
	for _, c := range a.Chars.For(h.owner) {
		if c.ID == "wren" {
			t.Fatal("Kim's private sprig is on the owner's roster")
		}
	}
	h.kimSays("ledger, what's in the accounts?")
	if p := lastChatReq(h.fm).Messages[0].Content; strings.HasPrefix(p, "You are Ledger") {
		t.Fatal("Kim reached the owner's private sprig")
	}

	// Inbox items.
	id, err := a.DB.AddInbox(ctx, store.InboxItem{UserID: h.owner.ID, Character: "hal", Kind: store.InboxDraft, Title: "Hal drafted: turn on", Action: `{"tool":"mcp_ha__turn_on","args":{}}`})
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := a.DB.OpenInbox(ctx, h.kim.ID); len(items) != 0 {
		t.Fatalf("Kim sees inbox items: %+v", items)
	}
	if err := a.Engine.RunDraft(ctx, h.kim, id); !errors.Is(err, agent.ErrNotYours) {
		t.Fatalf("Kim ran the owner's draft: %v", err)
	}

	// Kim can't raise a level on a family sprig, or on the owner's own,
	// in chat or through the API; each attempt is refused and logged.
	h.kimSays("set level mcp_ha__turn_on act for hal")
	h.kimSays("set level remember act for ledger")
	if err := a.Engine.SetToolLevel(ctx, h.kim, "hal", "mcp_ha__turn_on", store.LevelAct); err == nil {
		t.Fatal("Kim raised Hal's level through the API")
	}
	for _, sprig := range []string{"hal", "ledger"} {
		if lv, _ := a.DB.ExactToolLevel(ctx, sprig, "mcp_ha__turn_on"); lv != "" {
			t.Fatalf("%s's level changed to %s", sprig, lv)
		}
	}
	if lv, _ := a.DB.ExactToolLevel(ctx, "ledger", "remember"); lv != "" {
		t.Fatalf("ledger's level changed to %s", lv)
	}
	trail, _ := a.DB.Trail(ctx, 0, 50)
	refusals := 0
	for _, e := range trail {
		if e.Kind == store.TrailRefused && e.UserID == h.kim.ID {
			refusals++
			if e.Character == "ledger" && strings.Contains(e.Summary, "Ledger") {
				t.Fatalf("a refusal names the owner's private sprig: %+v", e)
			}
		}
	}
	if refusals != 3 {
		t.Fatalf("%d refusals in the trail, want 3: %+v", refusals, trail)
	}
	acts, _ := a.DB.RecentActivity(ctx, 100, "")
	if !hasActivity(acts, "refused: Kim tried to change a permission level on Hal") {
		t.Fatal("the refusal isn't in the activity log")
	}

	// The owner may (Hal is shared); Kim may on her own sprig.
	h.ownerSay("set level mcp_ha__turn_on act for hal")
	if lv, _ := a.DB.ExactToolLevel(ctx, "hal", "mcp_ha__turn_on"); lv != store.LevelAct {
		t.Fatalf("the owner couldn't set Hal's level: %q", lv)
	}
	h.kimSays("set level remember ask for wren")
	if lv, _ := a.DB.ExactToolLevel(ctx, "wren", "remember"); lv != store.LevelAsk {
		t.Fatalf("Kim couldn't set her own sprig's level: %q", lv)
	}
	h.ownerSay("set level remember act for wren")
	if lv, _ := a.DB.ExactToolLevel(ctx, "wren", "remember"); lv != store.LevelAsk {
		t.Fatal("the owner changed Kim's private sprig")
	}
	// Exec and paths are the owner's to give, too.
	wren, _ := a.Chars.Get("wren")
	if err := a.Engine.SetExec(ctx, h.owner, wren, true); err == nil {
		t.Fatal("the owner turned on exec for Kim's private sprig")
	}
	hal, _ := a.Chars.Get("hal")
	if err := a.Engine.GrantPath(ctx, h.kim, hal, t.TempDir()); err == nil {
		t.Fatal("Kim granted a path to a shared sprig")
	}
}

// A shared sprig's drafts go to the person whose request caused them.
func TestSharedSprigDraftsGoToTheRequester(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	if err := a.Engine.SetToolLevel(ctx, h.owner, "hal", "mcp_ha__turn_on", store.LevelDraft); err != nil {
		t.Fatal(err)
	}
	h.kimSays("hal, turn on the light")
	kims, _ := a.DB.OpenInbox(ctx, h.kim.ID)
	owners, _ := a.DB.OpenInbox(ctx, h.owner.ID)
	if len(kims) != 1 || kims[0].Kind != store.InboxDraft || len(owners) != 0 {
		t.Fatalf("Kim's inbox %+v, the owner's %+v", kims, owners)
	}
	if h.writes() != 0 {
		t.Fatal("a draft was done")
	}
}

func mustMemories(t *testing.T, a *App, user int64) []store.Memory {
	t.Helper()
	ms, err := a.DB.AllMemories(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// A level set for every sprig is household policy for shared sprigs: it
// doesn't reach a solo sprig, core tools included, so an admin can't
// change what someone's private sprig may do that way (issue 66). The
// solo sprig's own levels still apply.
func TestEverySprigLevelSkipsPrivateSprigs(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	writeSolo(t, a, "wren", h.kim.ID)

	// The owner (an admin) puts remember at ask for every sprig.
	if err := a.Engine.SetToolLevel(ctx, h.owner, store.AllSprigs, "remember", store.LevelAsk); err != nil {
		t.Fatal(err)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "juniper", "remember", store.LevelFor{}); lv != store.LevelAsk {
		t.Fatalf("a shared sprig's level = %q, want ask", lv)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "wren", "remember", store.LevelFor{Private: true}); lv != "" {
		t.Fatalf("Kim's private sprig's level = %q, want the default", lv)
	}
	// In practice: Juni asks; Kim's Wren just remembers.
	if out := h.ownerSay("remember my dog is Biscuit"); !strings.Contains(texts(out), "I need your OK") {
		t.Fatalf("Juni didn't ask: %+v", out)
	}
	if mems := mustMemories(t, a, h.owner.ID); len(mems) != 0 {
		t.Fatalf("Juni remembered without asking: %+v", mems)
	}
	h.kimSays("wren, remember my dog is Biscuit")
	if mems := mustMemories(t, a, h.kim.ID); len(mems) != 1 {
		t.Fatalf("Kim's private sprig was held by a household level: %+v", mems)
	}

	// Kim's own level for her sprig still applies.
	if err := a.Engine.SetToolLevel(ctx, h.kim, "wren", "remember", store.LevelRead); err != nil {
		t.Fatal(err)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "wren", "remember", store.LevelFor{Private: true}); lv != store.LevelRead {
		t.Fatalf("Kim's own level = %q, want read", lv)
	}
}
