package app

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func kidSprig(kid, parent int64) string {
	return fmt.Sprintf(`---
id: bramble
name: Bramble
role: A friendly companion for Max.
household: kid
kids: [%d]
parents: [%d]
personality:
  traits: [playful]
voice:
  style: [short and kind]
tools: ["mcp:ha/get_state", "mcp:ha/turn_on"]
---
Bramble likes games.
`, kid, parent)
}

func from(user, chat, text string) channel.Incoming {
	return channel.Incoming{Channel: "discord", ChatID: chat, MessageID: fmt.Sprint(len(text)) + text[:min(4, len(text))], UserID: user, UserName: user, Text: text, IsDM: true}
}

func toolNames(fm *fakeModel) map[string]bool {
	out := map[string]bool{}
	for _, d := range lastChatReq(fm).Tools {
		out[d.Function.Name] = true
	}
	return out
}

type household struct {
	a        *App
	fc       *fakeChannel
	fm       *fakeModel
	ctx      context.Context
	owner    *store.User // parent of Bramble
	kim      *store.User // another adult
	kid      *store.User
	reads    func() int32
	writes   func() int32
	kidSays  func(text string) []sent
	kimSays  func(text string) []sent
	ownerSay func(text string) []sent
}

func setupHousehold(t *testing.T) *household {
	t.Helper()
	a, fc, fm, ctx, reads, writes := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	kim, err := a.DB.CreateUser(ctx, "discord", "kim", "Kim", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := a.DB.CreateUser(ctx, "discord", "max", "Max", store.RoleKid)
	if err != nil {
		t.Fatal(err)
	}
	if !kid.IsKid() || kid.IsAdmin() || kim.IsKid() {
		t.Fatalf("roles: kid %+v kim %+v", kid, kim)
	}
	// Made through Thicket (as Juni or the API would), so it can be changed later.
	c, err := character.Parse([]byte(kidSprig(kid.ID, owner.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Chars.Write(ctx, c); err != nil {
		t.Fatal(err)
	}
	fc.take()
	say := func(user, chat string) func(string) []sent {
		return func(text string) []sent {
			a.Engine.HandleIncoming(ctx, from(user, chat, text))
			return fc.take()
		}
	}
	return &household{a: a, fc: fc, fm: fm, ctx: ctx, owner: owner, kim: kim, kid: kid,
		reads: reads.Load, writes: writes.Load,
		kidSays: say("max", "dm-max"), kimSays: say("kim", "dm-kim"), ownerSay: say("owner", "dm-owner")}
}

func personas(out []sent) string {
	var p []string
	for _, s := range out {
		p = append(p, s.persona)
	}
	return strings.Join(p, ",")
}

func TestKidSprigBoundaries(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx

	// Kids talk to their kid sprig, never to Juni or family sprigs.
	if out := h.kidSays("hi!"); personas(out) != "Bramble" {
		t.Fatalf("kid answered by %q", personas(out))
	}
	if out := h.kidSays("Juni, are you there?"); personas(out) != "Bramble" {
		t.Fatalf("kid reached the host: %q", personas(out))
	}
	if out := h.kidSays("Hal, check the light"); strings.Contains(personas(out), "Hal") {
		t.Fatal("kid reached a family sprig")
	}
	// Adults can't talk to the kid sprig.
	if out := h.kimSays("Bramble, hi"); strings.Contains(personas(out), "Bramble") {
		t.Fatal("an adult talked to a kid sprig")
	}

	// Only approved, read-only tools are offered; nothing that changes things.
	h.kidSays("hello again")
	tools := toolNames(h.fm)
	for _, must := range []string{"remember", "list_memories", "mcp_ha__get_state"} {
		if !tools[must] {
			t.Errorf("kid sprig missing %s: %v", must, tools)
		}
	}
	for _, never := range []string{"mcp_ha__turn_on", "schedule_job", "create_webhook", "create_trigger", "delegate_task",
		"set_followup", "request_access", "settings", "grant_tools", "calendar", "web_search"} {
		if tools[never] {
			t.Errorf("kid sprig offered %s", never)
		}
	}

	// Even with every level raised to act in the database, a change is refused.
	a.DB.SetToolLevel(ctx, "bramble", store.AnyTool, store.LevelAct)
	a.DB.SetToolLevel(ctx, "bramble", "mcp_ha__turn_on", store.LevelAct)
	a.DB.SetToolLevel(ctx, store.AllSprigs, "mcp_ha__turn_on", store.LevelAct)
	h.kidSays("turn on the light")
	if h.writes() != 0 {
		t.Fatal("a kid sprig changed something")
	}
	if pending, _ := a.DB.LatestPendingApproval(ctx, h.kid.ID, "discord:dm-max", 0); pending != nil {
		t.Fatal("a kid sprig asked for approval")
	}
	readsBefore := h.reads()
	h.kidSays("check the light")
	if h.reads() != readsBefore+1 {
		t.Fatalf("approved read-only tool didn't run: %d reads", h.reads())
	}
	// Levels can't be raised for it through Thicket either.
	if err := a.Engine.SetToolLevel(ctx, h.owner, "bramble", "mcp_ha__get_state", store.LevelAct); err == nil {
		t.Fatal("raised a kid sprig's level")
	}

	// A parent allowing the calendar gives it the read-only calendar, nothing that writes.
	if _, err := a.Engine.KidAccess(ctx, h.owner, "bramble", nil, nil, []string{"calendar"}, nil); err != nil {
		t.Fatal(err)
	}
	h.kidSays("what's on this week?")
	if tools := toolNames(h.fm); !tools["calendar"] || tools["add_to_calendar"] || tools["remove_from_calendar"] {
		t.Fatalf("calendar tools with calendar allowed: %v", tools)
	}

	// No routines: a job owned by the kid sprig refuses to run.
	j, err := a.DB.CreateJob(ctx, store.Job{UserID: h.kid.ID, Character: "bramble", Name: "Nightly", Schedule: "0 20 * * *", Prompt: "Say goodnight."})
	if err != nil {
		t.Fatal(err)
	}
	if res := a.Engine.RunJob(ctx, *j, 0); res.Err == nil || !strings.Contains(res.Err.Error(), "kid sprigs don't run routines") {
		t.Fatalf("kid job ran: %+v", res)
	}

	// Kids' messages stay for their parents.
	before, _ := a.Engine.KidChats(ctx, h.owner, "bramble", 0, 200)
	if out := h.kidSays("delete that"); len(out) == 0 || !strings.Contains(out[0].text, "stay put") {
		t.Fatalf("delete: %+v", out)
	}
	after, _ := a.Engine.KidChats(ctx, h.owner, "bramble", 0, 200)
	if len(after) < len(before) {
		t.Fatal("a kid deleted messages")
	}
}

func TestParentsReadKidChats(t *testing.T) {
	h := setupHousehold(t)
	h.kidSays("I beat level 3 today")
	msgs, err := h.a.Engine.KidChats(h.ctx, h.owner, "bramble", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, m := range msgs {
		saw = saw || (m.Role == "user" && strings.Contains(m.Content, "level 3") && m.KidID == h.kid.ID)
	}
	if !saw {
		t.Fatalf("parent can't see the kid's message: %+v", msgs)
	}
	for who, u := range map[string]*store.User{"another adult": h.kim, "the kid": h.kid} {
		if _, err := h.a.Engine.KidChats(h.ctx, u, "bramble", 0, 50); !errors.Is(err, agent.ErrNotParent) {
			t.Errorf("%s read the kid's chats: %v", who, err)
		}
	}
	if _, err := h.a.Engine.KidChats(h.ctx, h.owner, "hal", 0, 50); err == nil {
		t.Error("read a family sprig's chats as a kid sprig")
	}
}

func TestSafetyAlerts(t *testing.T) {
	h := setupHousehold(t)
	// Ordinary chat: no classifier call, nothing to parents.
	h.kidSays("what's the best skin in this game?")
	if h.fm.safetyCalls != 0 {
		t.Fatalf("classifier called on an ordinary message")
	}
	alerts := func() (owner, kim int) {
		for _, s := range h.fc.peek() {
			if strings.Contains(s.text, "Safety alert") {
				switch s.chat {
				case "dm-owner":
					owner++
				case "dm-kim":
					kim++
				}
			}
		}
		return
	}
	if o, k := alerts(); o+k != 0 {
		t.Fatal("routine notification to parents")
	}

	h.a.Engine.HandleIncoming(h.ctx, from("max", "dm-max", "a guy online asked where I live and my school"))
	if o, k := alerts(); o != 1 || k != 0 {
		t.Fatalf("stranger alert: owner %d, kim %d\n%s", o, k, sentText(h.fc.peek()))
	}
	h.fc.take()
	// Self-harm alerts even when the classifier is down.
	h.a.Engine.HandleIncoming(h.ctx, from("max", "dm-max", "fail screen: sometimes I want to die"))
	if o, _ := alerts(); o != 1 {
		t.Fatalf("self-harm alert with the classifier down: %d\n%s", o, sentText(h.fc.peek()))
	}
	trail, _ := h.a.DB.Trail(h.ctx, h.kid.ID, 10)
	n := 0
	for _, e := range trail {
		if e.Kind == store.TrailSafety && e.Character == "bramble" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("safety alerts in the trail: %d (%+v)", n, trail)
	}
}

func TestFamilyMemoryIsSeparate(t *testing.T) {
	h := setupHousehold(t)
	systemOf := func() string { return lastChatReq(h.fm).Messages[0].Content }

	h.ownerSay("Juni, remember my dog is called Biscuit") // private to the owner
	h.kimSays("Juni, remember we are vegetarian")         // family memory

	h.ownerSay("Juni, what's for dinner?")
	if s := systemOf(); !strings.Contains(s, "vegetarian") || !strings.Contains(s, "Biscuit") {
		t.Fatal("the owner should see family memory and their own")
	}
	h.kimSays("Juni, what's for dinner?")
	if s := systemOf(); !strings.Contains(s, "vegetarian") || strings.Contains(s, "Biscuit") {
		t.Fatal("Kim should see family memory but not the owner's private memory")
	}
	h.kidSays("what's for dinner?")
	if s := systemOf(); strings.Contains(s, "vegetarian") || strings.Contains(s, "Biscuit") {
		t.Fatal("a kid sprig saw adult data without a parent allowing it")
	}
	if mems, _ := h.a.DB.AllMemories(h.ctx, h.kim.ID); len(mems) != 0 {
		t.Fatalf("family memory leaked into Kim's private memories: %+v", mems)
	}

	// Only a parent can widen what the kid sprig sees, one approved item at a time.
	if _, err := h.a.Engine.KidAccess(h.ctx, h.kim, "bramble", nil, nil, []string{"family_memory"}, nil); err == nil {
		t.Fatal("a non-parent changed a kid sprig")
	}
	if _, err := h.a.Engine.KidAccess(h.ctx, h.owner, "bramble", []string{"mcp:ha/*"}, nil, nil, nil); err == nil {
		t.Fatal("a whole integration was granted to a kid sprig")
	}
	if _, err := h.a.Engine.KidAccess(h.ctx, h.owner, "bramble", nil, nil, []string{"family_memory"}, nil); err != nil {
		t.Fatal(err)
	}
	h.kidSays("what's for dinner?")
	if s := systemOf(); !strings.Contains(s, "vegetarian") || strings.Contains(s, "Biscuit") {
		t.Fatal("allowed family memory should show, private memory never")
	}
	// The change is in the trail, and the parent can undo it.
	trail, _ := h.a.DB.Trail(h.ctx, h.owner.ID, 10)
	if len(trail) == 0 || !trail[0].Undoable || !strings.Contains(trail[0].Summary, "may see") {
		t.Fatalf("trail: %+v", trail)
	}
	if _, err := h.a.Engine.UndoTrail(h.ctx, h.owner, trail[0].ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.a.Chars.Get("bramble"); len(c.Allow) != 0 {
		t.Fatalf("undo didn't take back the allowance: %v", c.Allow)
	}
}

func TestTrailUndo(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	if err := a.Engine.SetToolLevel(ctx, h.kim, "hal", "mcp_ha__turn_on", store.LevelAct); err == nil {
		t.Fatal("a member changed a level")
	}
	if err := a.Engine.SetToolLevel(ctx, h.owner, "hal", "mcp_ha__turn_on", store.LevelAct); err != nil {
		t.Fatal(err)
	}
	if lv, _ := a.DB.ExactToolLevel(ctx, "hal", "mcp_ha__turn_on"); lv != store.LevelAct {
		t.Fatalf("level: %q", lv)
	}
	trail, _ := a.DB.Trail(ctx, h.owner.ID, 1)
	if len(trail) != 1 || trail[0].Kind != store.TrailLevel || !trail[0].Undoable {
		t.Fatalf("trail: %+v", trail)
	}
	for who, u := range map[string]*store.User{"member": h.kim, "kid": h.kid} {
		if _, err := a.Engine.UndoTrail(ctx, u, trail[0].ID); err == nil {
			t.Fatalf("a %s undid a permission change", who)
		}
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, trail[0].ID); err != nil {
		t.Fatal(err)
	}
	if lv, _ := a.DB.ExactToolLevel(ctx, "hal", "mcp_ha__turn_on"); lv != "" {
		t.Fatalf("level after undo: %q", lv)
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, trail[0].ID); !errors.Is(err, agent.ErrAlreadyUndone) {
		t.Fatalf("undone twice: %v", err)
	}
}

func TestDashboardChanges(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	if err := a.Engine.SetToolLevel(ctx, h.owner, "hal", "mcp_ha__turn_on", store.LevelAct); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session, _ := a.DB.CreateWebSession(ctx, h.owner.ID, "test", web.SessionTTL)
	_, body := getDashboard(t, srv, session)
	if !strings.Contains(body, `id="changes"`) || !strings.Contains(body, "mcp_ha__turn_on for hal: default → act") || !strings.Contains(body, `value="undo"`) {
		t.Fatalf("changes not shown with an undo button:\n%s", body)
	}
	trail, _ := a.DB.Trail(ctx, h.owner.ID, 1)
	if code, _ := postAct(t, srv, session, url.Values{"csrf": {csrfFrom(t, body)}, "kind": {"trail"}, "id": {itoa(trail[0].ID)}, "action": {"undo"}}); code != 303 {
		t.Fatalf("undo from the dashboard: %d", code)
	}
	if lv, _ := a.DB.ExactToolLevel(ctx, "hal", "mcp_ha__turn_on"); lv != "" {
		t.Fatalf("level after dashboard undo: %q", lv)
	}
	// Kids don't get the changes list.
	ks, _ := a.DB.CreateWebSession(ctx, h.kid.ID, "test", web.SessionTTL)
	if _, kbody := getDashboard(t, srv, ks); strings.Contains(kbody, `id="changes"`) {
		t.Fatal("a kid saw the changes list")
	}
}
