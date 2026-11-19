package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

func TestWebAppHouseholdScreens(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	get := func(u *store.User, method, path string, body string) (int, map[string]any) {
		t.Helper()
		session, _ := a.DB.CreateWebSession(ctx, u.ID, "test", web.SessionTTL)
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "thicket_session", Value: session})
		req.Header.Set("X-Thicket", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := map[string]any{}
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	h.kidSays("hello bramble")

	// The sidebar's Household section comes from /api/me.
	_, me := get(h.owner, "GET", "/api/me", "")
	po := me["parent_of"].([]any)
	if len(po) != 1 || po[0].(map[string]any)["id"] != "bramble" || me["calendar"] != true {
		t.Fatalf("owner's /api/me: parent_of %v calendar %v", me["parent_of"], me["calendar"])
	}
	if kids := po[0].(map[string]any)["kids"].([]any); kids[0].(map[string]any)["name"] != "Max" {
		t.Fatalf("kid names: %v", kids)
	}
	_, kimMe := get(h.kim, "GET", "/api/me", "")
	if len(kimMe["parent_of"].([]any)) != 0 {
		t.Fatal("Kim isn't a parent of any kid sprig")
	}
	_, kidMe := get(h.kid, "GET", "/api/me", "")
	if kidMe["calendar"] != false || len(kidMe["parent_of"].([]any)) != 0 {
		t.Fatalf("kid's /api/me: %v", kidMe)
	}

	// The screens' calls, with the web app's session.
	if code, out := get(h.owner, "GET", "/api/calendar", ""); code != 200 || out["items"] == nil {
		t.Fatalf("calendar: %d %v", code, out)
	}
	if code, _ := get(h.kid, "GET", "/api/calendar", ""); code != 403 {
		t.Fatalf("kid calendar: %d", code)
	}
	code, out := get(h.owner, "GET", "/api/kid-sprigs/bramble/chats", "")
	msgs, _ := out["messages"].([]any)
	if code != 200 || len(msgs) == 0 {
		t.Fatalf("kid chats: %d %v", code, out)
	}
	if m := msgs[len(msgs)-1].(map[string]any); m["role"] != "user" || m["content"] != "hello bramble" || m["at"] == nil {
		t.Fatalf("kid chat message: %v", m)
	}
	if code, _ := get(h.kim, "GET", "/api/kid-sprigs/bramble/chats", ""); code != 403 {
		t.Fatalf("non-parent kid chats: %d", code)
	}
	if code, out := get(h.owner, "PATCH", "/api/kid-sprigs/bramble/access", `{"add_allow":["calendar"]}`); code != 200 || fmt.Sprint(out["allow"]) != "[calendar]" {
		t.Fatalf("kid access: %d %v", code, out)
	}
	// Writes need the web app's CSRF header, like every other write.
	session, _ := a.DB.CreateWebSession(ctx, h.owner.ID, "test", web.SessionTTL)
	req, _ := http.NewRequest("PATCH", srv.URL+"/api/kid-sprigs/bramble/access", strings.NewReader(`{"remove_allow":["calendar"]}`))
	req.AddCookie(&http.Cookie{Name: "thicket_session", Value: session})
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("write without X-Thicket: %d", res.StatusCode)
	}
}

// Juni sets up a kid sprig in chat: an interview, a preview in its own
// voice, a parent preview to approve, and nothing before approval.
func TestKidSprigInChat(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	h.kidSays("hi!") // Max has talked before, so he can be reached

	// Only admins are offered the tool, and only admins can create one.
	h.kimSays("hello")
	if toolNames(h.fm)["create_kid_sprig"] {
		t.Fatal("a member was offered create_kid_sprig")
	}
	if _, err := a.Engine.CreateKidSprig(ctx, h.kim, agent.KidSprigSpec{ID: "sneaky", Name: "Sneaky", Role: "x", Kids: []int64{h.kid.ID}, Parents: []int64{h.kim.ID}}); err == nil {
		t.Fatal("a member created a kid sprig")
	}
	h.ownerSay("hello")
	if !toolNames(h.fm)["create_kid_sprig"] {
		t.Fatal("the owner wasn't offered create_kid_sprig")
	}

	// An adult can't be listed as a kid.
	out := h.ownerSay("make a kid sprig clover for Kim")
	if strings.Contains(personas(out), "Clover") || !strings.Contains(lastToolResult(h.fm), "isn't one of the household's kids") {
		t.Fatalf("an adult listed as a kid: %q / %s", personas(out), lastToolResult(h.fm))
	}

	// Never in a group channel: the preview names the household's kids.
	a.Engine.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "general", MessageID: "g1", UserID: "owner", UserName: "owner",
		Text: "make a kid sprig clover for Max", Mentioned: true})
	if out := h.fc.take(); strings.Contains(personas(out), "Clover") || !strings.Contains(lastToolResult(h.fm), "in a DM") {
		t.Fatalf("set up in a group channel: %+v / %s", out, lastToolResult(h.fm))
	}

	// The preview says hello as it would to the kid.
	out = h.ownerSay("make a kid sprig clover for Max with parents me,Kim")
	if !strings.Contains(personas(out), "Clover (preview)") || !strings.Contains(sentText(out), "doodling") {
		t.Fatalf("no preview: %+v", out)
	}
	out = h.ownerSay("lovely, create the kid sprig clover")
	text := sentText(out)
	for _, want := range []string{"Parent preview", "Clover", "Talks only to Max", "and Kim. They can read every chat", "no tools", "Reply **approve**"} {
		if !strings.Contains(text, want) {
			t.Fatalf("parent preview missing %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(lastToolResult(h.fm), "PENDING") {
		t.Fatalf("model told: %q", lastToolResult(h.fm))
	}
	// Nothing is created before approval.
	if _, ok := a.Chars.Get("clover"); ok {
		t.Fatal("clover created before approval")
	}

	// Only the cap the parent preview showed: if it changed, nothing is created.
	capWas := a.Settings.String(ctx, "budget.kid_monthly_usd")
	a.Settings.Set(ctx, "budget.kid_monthly_usd", "25")
	h.ownerSay("approve")
	if _, ok := a.Chars.Get("clover"); ok {
		t.Fatal("created with a cap the preview didn't show")
	}
	a.Settings.Set(ctx, "budget.kid_monthly_usd", capWas)
	h.ownerSay("create the kid sprig clover")

	out = h.ownerSay("approve")
	c, ok := a.Chars.Get("clover")
	if !ok || !c.IsKid() || len(c.Tools) != 0 || len(c.Allow) != 0 {
		t.Fatalf("clover after approval: %+v", c)
	}
	if fmt.Sprint(c.Kids) != fmt.Sprint([]int64{h.kid.ID}) || fmt.Sprint(c.Parents) != fmt.Sprint([]int64{h.owner.ID, h.kim.ID}) {
		t.Fatalf("kids %v parents %v", c.Kids, c.Parents)
	}
	if !modelSaw(h.fm, "Clover's chats") {
		t.Fatal("the approving parent wasn't told what they can do")
	}
	var kimTold, kidHello bool
	for _, s := range out {
		kimTold = kimTold || (s.chat == "dm-kim" && strings.Contains(s.text, "Clover's chats") && strings.Contains(s.text, "safety alert"))
		kidHello = kidHello || (s.chat == "dm-max" && s.persona == "Clover" && strings.Contains(s.text, "Clover"))
	}
	if !kimTold || !kidHello {
		t.Fatalf("kim told %v, kid hello %v:\n%+v", kimTold, kidHello, out)
	}
	trail, _ := a.DB.Trail(ctx, h.owner.ID, 20)
	var inTrail bool
	for _, e := range trail {
		inTrail = inTrail || (e.Character == "clover" && strings.Contains(e.Summary, "kid sprig"))
	}
	if !inTrail {
		t.Fatalf("not in the trail: %+v", trail)
	}
}
