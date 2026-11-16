package app

// Phase 2, end to end: onboarding a sprig through an approved diff, trust
// upgrades, live avatar state, the dashboard's inbox, the commons, and
// event triggers.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func TestOnboardingShowsADiffAndWaitsForApproval(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	dir := a.Cfg.CharactersDir
	a.Engine.HandleIncoming(ctx, dm("bring on wren please"))
	previewed := false
	for _, s := range fc.take() {
		previewed = previewed || s.persona == "Wren (preview)"
	}
	if !previewed {
		t.Fatal("the new sprig didn't introduce itself")
	}
	a.Engine.HandleIncoming(ctx, dm("lovely, create wren"))
	out := sentText(fc.take())
	for _, want := range []string{"```diff", "+++ b/characters/wren.md", "+id: wren", "+++ b/characters/wren/avatar.json", `+  "accessory": "book"`,
		"+++ b/characters/wren/voice_tests/01-hello.yaml", "Sunday reading nudge", "**draft**", "Reply **approve**"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff message missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(lastToolResult(fm), "PENDING") {
		t.Fatalf("model told: %q", lastToolResult(fm))
	}
	if _, err := os.Stat(filepath.Join(dir, "wren.md")); err == nil {
		t.Fatal("the sprig went live before approval")
	}
	if _, ok := a.Chars.Get("wren"); ok {
		t.Fatal("wren loaded before approval")
	}

	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	wren, ok := a.Chars.Get("wren")
	if !ok {
		t.Fatal("wren not created after approval")
	}
	if look := avatar.Load(dir, "wren", wren.Color); look.Body != "tall" || look.Accessory != "book" || look.Expression != "curious" {
		t.Fatalf("avatar not written as approved: %+v", look)
	}
	if vt, _ := character.LoadVoiceTests(dir, "wren"); len(vt) != 3 || vt[0].Reference != "" {
		t.Fatalf("voice tests: %+v", vt)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "wren", "schedule_job", true); lv != store.LevelDraft {
		t.Fatalf("new sprig's tools start at %q, want draft", lv)
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	jobs, _ := a.DB.UserJobs(ctx, owner.ID)
	if len(jobs) != 1 || jobs[0].Character != "wren" || jobs[0].Schedule != "0 10 * * 0" {
		t.Fatalf("routine not scheduled: %+v", jobs)
	}

	// At draft, Wren writes a job up instead of scheduling it...
	a.Engine.HandleIncoming(ctx, dm("wren, schedule a reminder"))
	fc.take()
	if jobs, _ := a.DB.UserJobs(ctx, owner.ID); len(jobs) != 1 {
		t.Fatalf("a sprig at draft scheduled a job: %+v", jobs)
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxDraft || !strings.Contains(items[0].Detail, "stretch") {
		t.Fatalf("draft not in inbox: %+v", items)
	}
	// ...but still remembers things (core tools ignore the sprig-wide level).
	a.Engine.HandleIncoming(ctx, dm("wren, remember my dog is called Biscuit"))
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatalf("draft level blocked memory: %+v", mems)
	}
}

// A conflict during creation undoes what was made (never the user's own
// file), reports nothing created, and the same proposal works once fixed.
func TestOnboardingUndoesOnFailure(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	dir := a.Cfg.CharactersDir
	theirs := filepath.Join(dir, "wren", "voice_tests", "01-hello.yaml")
	os.MkdirAll(filepath.Dir(theirs), 0o755)
	os.WriteFile(theirs, []byte("prompt: mine\n"), 0o644)
	a.Engine.HandleIncoming(ctx, dm("bring on wren please"))
	a.Engine.HandleIncoming(ctx, dm("create wren"))
	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if _, ok := a.Chars.Get("wren"); ok {
		t.Fatal("created despite the conflict")
	}
	if b, _ := os.ReadFile(theirs); string(b) != "prompt: mine\n" {
		t.Fatal("the user's file was changed")
	}
	for _, f := range []string{"wren.md", "wren/avatar.json", "wren/voice_tests/02-what-do-you-do.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s left behind", f)
		}
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if jobs, _ := a.DB.UserJobs(ctx, owner.ID); len(jobs) != 0 {
		t.Fatalf("routines left behind: %+v", jobs)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "wren", "schedule_job", true); lv != "" {
		t.Fatalf("level left behind: %q", lv)
	}
	os.Remove(theirs)
	a.Engine.HandleIncoming(ctx, dm("create wren"))
	a.Engine.HandleIncoming(ctx, dm("approve"))
	if _, ok := a.Chars.Get("wren"); !ok {
		t.Fatal("the same proposal didn't work once the conflict was gone")
	}
}

func TestOnboardingDenied(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	a.Engine.HandleIncoming(ctx, dm("bring on wren please"))
	a.Engine.HandleIncoming(ctx, dm("create wren"))
	a.Engine.HandleIncoming(ctx, dm("deny"))
	fc.take()
	if _, err := os.Stat(filepath.Join(a.Cfg.CharactersDir, "wren.md")); err == nil {
		t.Fatal("a denied sprig was created")
	}
	if _, err := os.Stat(filepath.Join(a.Cfg.CharactersDir, "wren")); err == nil {
		t.Fatal("a denied sprig's folder was written")
	}
}

func TestTrustUpgrade(t *testing.T) {
	a, fc, _, ctx, _, writes := ready(t)
	a.DB.SetToolLevel(ctx, "hal", store.AnyTool, store.LevelDraft)
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	if writes.Load() != 0 {
		t.Fatal("ran at draft")
	}
	fc.take()

	a.Engine.HandleIncoming(ctx, dm("hal, ask for more trust"))
	out := fc.take()
	var prompt sent
	for _, s := range out {
		if strings.Contains(s.text, "asking for more trust") {
			prompt = s
		}
	}
	if prompt.persona != "Juni" {
		t.Fatalf("Juni should present the request: %+v", out)
	}
	for _, want := range []string{"draft → **act**", "porch light", "drafts written: 2", "approvals: none asked yet", "mistakes: none"} {
		if !strings.Contains(prompt.text, want) {
			t.Fatalf("request missing %q:\n%s", want, prompt.text)
		}
	}
	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if lv, _ := a.DB.ToolLevel(ctx, "hal", "mcp_ha__turn_on", true); lv != store.LevelAct {
		t.Fatalf("level after approval: %q", lv)
	}
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	if writes.Load() != 1 {
		t.Fatal("raised level didn't take effect")
	}
}

func TestTrustRequestRefusedWhenAlreadyThere(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", store.LevelAct)
	a.Engine.HandleIncoming(ctx, dm("hal, ask for more trust"))
	if !strings.Contains(lastToolResult(fm), "already at act") || strings.Contains(sentText(fc.take()), "asking for more trust") {
		t.Fatalf("got %q", lastToolResult(fm))
	}
}

func TestSprigStates(t *testing.T) {
	a, _, _, ctx, _, _ := ready(t)
	e := a.Engine
	noon := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return noon }
	if st := e.SprigStates(ctx); st["juniper"] != "idle" || st["hal"] != "idle" {
		t.Fatalf("idle: %v", st)
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.AddInbox(ctx, store.InboxItem{UserID: owner.ID, Character: "hal", Kind: store.InboxFailed, Ref: "job:9", Title: "broke"})
	if st := e.SprigStates(ctx); st["hal"] != "stuck" {
		t.Fatalf("stuck: %v", st)
	}
	night := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC) // Juni's quiet hours, owner on UTC
	e.Now = func() time.Time { return night }
	if st := e.SprigStates(ctx); st["juniper"] != "sleeping" {
		t.Fatalf("sleeping: %v", st)
	}
	svg, ok := a.Avatar(ctx, "juniper", "")
	if !ok || !strings.Contains(svg, `class="sprig is-sleeping"`) || !strings.Contains(svg, "@keyframes") {
		t.Fatalf("live avatar: %s", svg)
	}
	if _, ok := a.Avatar(ctx, "nobody", ""); ok {
		t.Fatal("avatar for an unknown sprig")
	}
}

func postAct(t *testing.T, srv *httptest.Server, session string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/dashboard/act", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: session})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<meta name="thicket-csrf" content="`)
	if !ok {
		t.Fatal("no form token on the page")
	}
	tok, _, _ := strings.Cut(rest, `"`)
	return tok
}

func TestDashboardInbox(t *testing.T) {
	a, fc, _, ctx, _, writes := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	session, _ := a.DB.CreateWebSession(ctx, owner.ID, "test", web.SessionTTL)
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()

	// An approval waiting in chat can be answered here.
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	fc.take()
	resp, body := getDashboard(t, srv, session)
	if resp.StatusCode != 200 || !strings.Contains(body, "wants to: <b>mcp_ha__turn_on</b>") {
		t.Fatalf("approval not shown:\n%s", body)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Fatal("script isn't locked to a nonce")
	}
	tok := csrfFrom(t, body)
	apps, _ := a.DB.PendingApprovals(ctx, owner.ID, time.Hour)
	id := apps[0].ID

	if code, _ := postAct(t, srv, session, url.Values{"csrf": {"forged"}, "kind": {"approval"}, "id": {itoa(id)}, "action": {"approve"}}); code != 403 {
		t.Fatalf("forged form accepted: %d", code)
	}
	member, _ := a.DB.CreateUser(ctx, "discord", "m1", "Kim", "member")
	ms, _ := a.DB.CreateWebSession(ctx, member.ID, "test", web.SessionTTL)
	_, mbody := getDashboard(t, srv, ms)
	if code, _ := postAct(t, srv, ms, url.Values{"csrf": {csrfFrom(t, mbody)}, "kind": {"approval"}, "id": {itoa(id)}, "action": {"approve"}}); code != 400 || writes.Load() != 0 {
		t.Fatalf("someone else answered the owner's request: %d", code)
	}
	code, _ := postAct(t, srv, session, url.Values{"csrf": {tok}, "kind": {"approval"}, "id": {itoa(id)}, "action": {"approve"}})
	if code != 303 || writes.Load() != 1 {
		t.Fatalf("approve from dashboard: %d, writes %d", code, writes.Load())
	}
	if out := fc.take(); len(out) == 0 || out[len(out)-1].persona != "Hal" {
		t.Fatalf("Hal didn't report back in chat: %+v", out)
	}
	if code, body := postAct(t, srv, session, url.Values{"csrf": {tok}, "kind": {"approval"}, "id": {itoa(id)}, "action": {"approve"}}); code != 400 || !strings.Contains(body, "already answered") {
		t.Fatalf("double approve: %d", code)
	}

	// A draft can be done from here; another cleared.
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", store.LevelDraft)
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	fc.take()
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 2 {
		t.Fatalf("drafts: %+v", items)
	}
	_, body = getDashboard(t, srv, session)
	if !strings.Contains(body, `value="run">Do it`) {
		t.Fatal("no Do it button for a draft")
	}
	if code, _ := postAct(t, srv, session, url.Values{"csrf": {tok}, "kind": {"inbox"}, "id": {itoa(items[0].ID)}, "action": {"run"}}); code != 303 || writes.Load() != 2 {
		t.Fatalf("run draft: %d, writes %d", code, writes.Load())
	}
	if code, _ := postAct(t, srv, session, url.Values{"csrf": {tok}, "kind": {"inbox"}, "id": {itoa(items[1].ID)}, "action": {"resolve"}}); code != 303 || writes.Load() != 2 {
		t.Fatalf("dismiss draft: %d", code)
	}
	if items, _ := a.DB.OpenInbox(ctx, owner.ID); len(items) != 0 {
		t.Fatalf("inbox not cleared: %+v", items)
	}

	// Live states for the page's poller.
	req, _ := http.NewRequest("GET", srv.URL+"/dashboard/state.json", nil)
	req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: session})
	r, _ := http.DefaultClient.Do(req)
	var st map[string]string
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st["hal"] == "" || st["juniper"] == "" {
		t.Fatalf("state.json: %v", st)
	}
}

func commonsMsg(text string) channel.Incoming {
	return channel.Incoming{Channel: "discord", ChatID: "commons", MessageID: text, UserID: "owner", UserName: "Sam", Text: text, Listening: true}
}

func TestCommonsHandoffByMention(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644)
	a.Chars.Load()

	// Outside the commons, a mention is just text.
	a.Engine.HandleIncoming(ctx, dm("juni, who handles calendars?"))
	if out := fc.take(); len(out) != 1 {
		t.Fatalf("handed off outside the commons: %+v", out)
	}
	a.Cfg.CommonsRef = "discord:commons"
	a.Engine.HandleIncoming(ctx, commonsMsg("juni, who handles calendars?"))
	out := fc.take()
	if len(out) != 2 || out[0].persona != "Juni" || out[1].persona != "Pip" || out[1].chat != "commons" {
		t.Fatalf("commons handoff: %+v", out)
	}
	acts, _ := a.DB.RecentActivity(ctx, 20, "info")
	logged := false
	for _, ac := range acts {
		logged = logged || (ac.Kind == "handoff" && ac.Summary == "Juni handed off to Pip")
	}
	if !logged {
		t.Fatal("handoff not in the trail")
	}
	// The commons prompt tells sprigs how it works.
	fm.mu.Lock()
	sys := fm.reqs[len(fm.reqs)-1].Messages[0].Content
	fm.mu.Unlock()
	if !strings.Contains(sys, "## The commons") {
		t.Fatal("the commons isn't explained in the prompt")
	}
}

func TestIntegrationTrigger(t *testing.T) {
	a, fc, fm, ctx, reads, _ := ready(t)
	e := a.Engine
	e.HandleIncoming(ctx, dm("watch the switch"))
	if !strings.Contains(lastToolResult(fm), "only reads") {
		t.Fatalf("a trigger on a tool that acts was allowed: %q", lastToolResult(fm))
	}
	e.HandleIncoming(ctx, dm("watch the lights"))
	if !strings.Contains(lastToolResult(fm), "Trigger job #") {
		t.Fatalf("create_trigger: %q", lastToolResult(fm))
	}
	fc.take()
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	jobs, _ := a.DB.UserJobs(ctx, owner.ID)
	if len(jobs) != 1 {
		t.Fatalf("jobs: %+v", jobs)
	}
	j := jobs[0]
	now := time.Now()
	e.Now = func() time.Time { return now }
	a.DB.SetClock(func() time.Time { return now })
	a.Scheduler.Now = func() time.Time { return now }
	poll := func(state string) {
		haState.Store(state)
		now = now.Add(6 * time.Minute)
		e.PollTriggers(ctx)
		a.Scheduler.Wait()
	}

	poll(`[{"id":"kitchen","state":"on"},{"id":"porch","state":"off"}]`) // first look: nothing fires
	if runs, _ := a.DB.JobRuns(ctx, j.ID, 5); len(runs) != 0 {
		t.Fatalf("fired on what was already there: %+v", runs)
	}
	poll(`[{"id":"kitchen","state":"on"},{"id":"porch","state":"off"},{"id":"hall","state":"off"}]`) // new, but no match
	if runs, _ := a.DB.JobRuns(ctx, j.ID, 5); len(runs) != 0 {
		t.Fatalf("fired on a non-matching item: %+v", runs)
	}
	before := len(fm.reqs)
	poll(`[{"id":"kitchen","state":"on"},{"id":"porch","state":"off"},{"id":"hall","state":"off"},{"id":"garage","state":"on"}]`)
	runs, _ := a.DB.JobRuns(ctx, j.ID, 5)
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("didn't fire on a new match: %+v", runs)
	}
	var task string
	for _, r := range fm.reqs[before:] {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "[Trigger job") {
				task = m.Content
			}
		}
	}
	if !strings.Contains(task, "garage") || strings.Contains(task, "kitchen") || !strings.Contains(task, "<untrusted") {
		t.Fatalf("job got: %q", task)
	}
	if out := fc.take(); len(out) == 0 || out[len(out)-1].persona != "Hal" {
		t.Fatalf("result not delivered: %+v", out)
	}
	// Too soon: no poll (engine and store share the test clock).
	n := reads.Load()
	now = now.Add(time.Minute)
	e.PollTriggers(ctx)
	if reads.Load() != n {
		t.Fatal("polled before its interval")
	}

	// A broken integration is dead-lettered, once.
	a.MCP.Disconnect("ha")
	a.DB.DeleteMCPServer(ctx, "ha")
	poll("")
	poll("")
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Count != 2 || !strings.Contains(items[0].Title, "on hold") || !strings.Contains(items[0].Detail, "isn't available") {
		t.Fatalf("poll failure: %+v", items)
	}
}

// A watched resource whose state changes fires; more than a batch of new
// items are delivered over several runs; a sprig that loses access stops
// polling.
func TestTriggerStateChangesBatchesAndAccess(t *testing.T) {
	a, _, fm, ctx, reads, _ := ready(t)
	e := a.Engine
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "hal", Name: "Containers", Prompt: "Tell me what's unhealthy.", Kind: store.JobWebhook})
	a.DB.PutTrigger(ctx, store.Trigger{JobID: j.ID, Source: store.TriggerMCP, Server: "ha", Tool: "get_state", EveryMin: 5, Match: "unhealthy"})
	now := time.Now()
	e.Now = func() time.Time { return now }
	a.DB.SetClock(func() time.Time { return now })
	a.Scheduler.Now = func() time.Time { return now }
	poll := func(state string) {
		haState.Store(state)
		now = now.Add(6 * time.Minute)
		e.PollTriggers(ctx)
		a.Scheduler.Wait()
	}
	runs := func() int { r, _ := a.DB.JobRuns(ctx, j.ID, 50); return len(r) }

	poll(`[{"id":"db","state":"healthy"}]`)
	poll(`[{"id":"db","state":"healthy"}]`)
	if runs() != 0 {
		t.Fatal("fired with nothing new")
	}
	poll(`[{"id":"db","state":"unhealthy"}]`)
	if runs() != 1 {
		t.Fatal("a container turning unhealthy didn't fire")
	}
	poll(`[{"id":"db","state":"healthy"}]`)
	poll(`[{"id":"db","state":"unhealthy"}]`)
	if runs() != 2 {
		t.Fatal("turning unhealthy a second time didn't fire")
	}

	// 25 new matches: 20 now, the other 5 next time, none lost.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 25; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"c` + strconv.Itoa(i) + `","state":"unhealthy"}`)
	}
	b.WriteString("]")
	before := len(fm.reqs)
	poll(b.String())
	now = now.Add(2 * time.Minute) // the webhook-style one-a-minute limit
	poll(b.String())
	if runs() != 4 {
		t.Fatalf("batches: %d runs", runs())
	}
	var delivered int
	for _, r := range fm.reqs[before:] {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "[Trigger job") {
				delivered += strings.Count(m.Content, `"state":"unhealthy"`)
			}
		}
	}
	if delivered != 25 {
		t.Fatalf("delivered %d of 25 items", delivered)
	}

	// Access removed: no more polling, and the user is told.
	os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "hal.md"), []byte(strings.Replace(hal, `tools: ["mcp:ha"]`, "tools: []", 1)), 0o644)
	a.Chars.Load()
	n := reads.Load()
	poll(`[{"id":"new","state":"unhealthy"}]`)
	if reads.Load() != n {
		t.Fatal("polled an integration the sprig no longer has")
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) == 0 || !strings.Contains(items[0].Detail, "no longer has access") {
		t.Fatalf("not told: %+v", items)
	}
}

func TestWebhookFilter(t *testing.T) {
	a, _, _, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	token := "thk_" + strings.Repeat("a", 32)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "hal", Name: "Containers", Prompt: "Tell me which container is unhealthy.", Kind: store.JobWebhook, HookHash: agent.HookHash(token)})
	a.DB.PutTrigger(ctx, store.Trigger{JobID: j.ID, Source: store.TriggerWebhook, Match: `/status\W+unhealthy/`})
	if code, msg := a.Webhook(ctx, token, []byte(`{"container":"db","status":"healthy"}`), http.Header{}); code != 200 || !strings.Contains(msg, "ignored") {
		t.Fatalf("non-matching body: %d %s", code, msg)
	}
	if code, _ := a.Webhook(ctx, token, []byte(`{"container":"db","status": "unhealthy"}`), http.Header{}); code != 202 {
		t.Fatalf("matching body: %d", code)
	}
	a.Scheduler.Wait()
	if runs, _ := a.DB.JobRuns(ctx, j.ID, 5); len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	// At the spend cap, event jobs are held, not failed.
	a.Settings.Set(ctx, "budget.monthly_usd", "0.000001")
	if code, msg := a.Webhook(ctx, token, []byte(`status: unhealthy`), http.Header{}); code != 503 || !strings.Contains(msg, "spend cap") {
		t.Fatalf("at the cap: %d %s", code, msg)
	}
}

// Review fixes: an approval whose sprig was removed isn't run as anyone
// else; a trust upgrade answered after the level changed doesn't apply;
// a webhook filter sees the body, not the headers; /health hides MCP
// error details.
func TestApprovalEdgeCases(t *testing.T) {
	a, fc, fm, ctx, _, writes := ready(t)
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	fc.take()
	os.Remove(filepath.Join(a.Cfg.CharactersDir, "hal.md"))
	a.Chars.Load()
	before := fm.calls
	a.Engine.HandleIncoming(ctx, dm("approve"))
	out := fc.take()
	if fm.calls != before {
		t.Fatal("the report ran a model turn, which could call tools")
	}
	if writes.Load() != 0 || len(out) != 1 || out[0].persona != "Juni" || !strings.Contains(out[0].text, "didn't run") {
		t.Fatalf("a removed sprig's approval ran (writes %d): %+v", writes.Load(), out)
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if apps, _ := a.DB.PendingApprovals(ctx, owner.ID, time.Hour); len(apps) != 0 {
		t.Fatal("approval left pending")
	}
}

func TestStaleTrustUpgradeIsRefused(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	a.DB.SetToolLevel(ctx, "hal", store.AnyTool, store.LevelDraft)
	a.Engine.HandleIncoming(ctx, dm("hal, ask for more trust"))
	fc.take()
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", store.LevelRead) // lowered meanwhile
	a.Engine.HandleIncoming(ctx, dm("approve"))
	if lv, _ := a.DB.ToolLevel(ctx, "hal", "mcp_ha__turn_on", true); lv != store.LevelRead {
		t.Fatalf("a stale request raised the level to %q", lv)
	}
	if err := a.DB.SetToolLevel(ctx, store.AllSprigs, store.AnyTool, store.LevelRead); err == nil {
		t.Fatal(`a level for every sprig and every tool was stored`)
	}
}

func TestWebhookFilterSeesTheBody(t *testing.T) {
	a, _, _, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	token := "thk_" + strings.Repeat("b", 32)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "hal", Name: "Down", Prompt: "x", Kind: store.JobWebhook, HookHash: agent.HookHash(token)})
	a.DB.PutTrigger(ctx, store.Trigger{JobID: j.ID, Source: store.TriggerWebhook, Match: `/^down$/`})
	h := http.Header{"Content-Type": {"text/plain"}}
	if code, msg := a.Webhook(ctx, token, []byte("down"), h); code != 202 {
		t.Fatalf("headers spoiled the match: %d %s", code, msg)
	}
	a.Scheduler.Wait()
}

func TestHealthHidesIntegrationErrors(t *testing.T) {
	a, _, _, ctx, _, _ := ready(t)
	s := store.MCPServer{Name: "broken", Transport: "http", Target: "http://127.0.0.1:1/mcp?token=sekrit", Enabled: true}
	a.DB.PutMCPServer(ctx, s)
	a.MCP.Connect(ctx, s)
	c := a.health(ctx)["mcp.broken"]
	if c.OK || strings.Contains(c.Detail, "sekrit") || strings.Contains(c.Detail, "127.0.0.1") {
		t.Fatalf("health detail: %+v", c)
	}
}
