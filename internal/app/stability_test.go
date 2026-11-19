package app

// Phase 1.5 behaviour, end to end with the fake chat app and model:
// permission levels, the heartbeat-to-task handoff, the needs-you inbox,
// restarts, memory, and voice tests.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// fakeHA is an MCP server with one tool that reads and one that acts.
// haState is what get_state returns (default "light is off").
var haState atomic.Value

// haCalls records the body of every tools/call the fake HA receives.
var haCalls struct {
	sync.Mutex
	bodies []string
}

func fakeHA(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	haState.Store("light is off")
	haCalls.Lock()
	haCalls.bodies = nil
	haCalls.Unlock()
	var reads, writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[`+
				`{"name":"get_state","description":"read a light","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},`+
				`{"name":"turn_on","description":"turn a light on","inputSchema":{"type":"object"}}]}}`, *req.ID)
		case "tools/call":
			haCalls.Lock()
			haCalls.bodies = append(haCalls.bodies, string(b))
			haCalls.Unlock()
			out := haState.Load().(string)
			if req.Params.Name == "turn_on" {
				writes.Add(1)
				out = "light is on"
			} else {
				reads.Add(1)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}]}}`, *req.ID, out)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reads, &writes
}

const hal = `---
id: hal
name: Hal
role: Looks after the house.
personality:
  traits: [calm]
voice:
  style: [short and plain]
tools: ["mcp:ha"]
---
Hal minds the lights.
`

// ready gets past first-run setup and adds Hal with the fake integration.
func ready(t *testing.T) (*App, *fakeChannel, *fakeModel, context.Context, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	a, fc, fm, ctx := setup(t)
	a.Engine.HandleIncoming(ctx, dm("hi"))
	a.Engine.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ab12", 16)))
	fc.take()
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "hal.md"), []byte(hal), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	srv, reads, writes := fakeHA(t)
	s := store.MCPServer{Name: "ha", Transport: "http", Target: srv.URL, Enabled: true}
	if err := a.DB.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MCP.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	return a, fc, fm, ctx, reads, writes
}

func sentText(out []sent) string {
	var b strings.Builder
	for _, s := range out {
		b.WriteString(s.text + "\n")
	}
	return b.String()
}

func TestPermissionLevels(t *testing.T) {
	cases := []struct {
		name        string
		all, sprig  string // levels for every sprig and for Hal ("" = unset)
		msg         string
		wantWrites  int32
		wantReads   int32
		wantPrompt  bool // an approval prompt was shown
		wantDraft   bool
		wantRefusal bool
	}{
		{name: "default asks before changing things", msg: "hal, turn on the light", wantPrompt: true},
		{name: "default reads freely", msg: "hal, check the light", wantReads: 1},
		{name: "act runs", sprig: "act", msg: "hal, turn on the light", wantWrites: 1},
		{name: "ask asks", sprig: "ask", msg: "hal, turn on the light", wantPrompt: true},
		{name: "ask asks even to read", sprig: "ask", msg: "hal, check the light", wantPrompt: true},
		{name: "draft writes it up instead", sprig: "draft", msg: "hal, turn on the light", wantDraft: true},
		{name: "draft may read", sprig: "draft", msg: "hal, check the light", wantReads: 1},
		{name: "read refuses changes", sprig: "read", msg: "hal, turn on the light", wantRefusal: true},
		{name: "read may read", sprig: "read", msg: "hal, check the light", wantReads: 1},
		{name: "a sprig's own level beats everyone's", all: "act", sprig: "read", msg: "hal, turn on the light", wantRefusal: true},
		{name: "everyone's level applies", all: "act", msg: "hal, turn on the light", wantWrites: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, fc, fm, ctx, reads, writes := ready(t)
			for who, lv := range map[string]string{store.AllSprigs: c.all, "hal": c.sprig} {
				for _, tool := range []string{"mcp_ha__turn_on", "mcp_ha__get_state"} {
					if err := a.DB.SetToolLevel(ctx, who, tool, lv); err != nil {
						t.Fatal(err)
					}
				}
			}
			a.Engine.HandleIncoming(ctx, dm(c.msg))
			out := sentText(fc.take())
			if writes.Load() != c.wantWrites || reads.Load() != c.wantReads {
				t.Fatalf("writes %d reads %d, want %d/%d", writes.Load(), reads.Load(), c.wantWrites, c.wantReads)
			}
			if got := strings.Contains(out, "I need your OK"); got != c.wantPrompt {
				t.Fatalf("approval prompt shown=%v, want %v:\n%s", got, c.wantPrompt, out)
			}
			owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
			items, _ := a.DB.OpenInbox(ctx, owner.ID)
			drafted := len(items) == 1 && items[0].Kind == store.InboxDraft && strings.Contains(items[0].Detail, "light.kitchen")
			if drafted != c.wantDraft {
				t.Fatalf("draft in inbox=%v, want %v: %+v", drafted, c.wantDraft, items)
			}
			var toolResult string
			for _, r := range fm.reqs {
				if last := r.Messages[len(r.Messages)-1]; last.Role == "tool" {
					toolResult = last.Content
				}
			}
			if got := strings.Contains(toolResult, "only have read access"); got != c.wantRefusal {
				t.Fatalf("refusal=%v, want %v (tool said %q)", got, c.wantRefusal, toolResult)
			}
			if c.wantDraft && !strings.HasPrefix(toolResult, "DRAFTED") {
				t.Fatalf("the model wasn't told it was only drafted: %q", toolResult)
			}
		})
	}
}

// Lowering a level while an approval waits means approving doesn't run it.
func TestApprovalRespectsALoweredLevel(t *testing.T) {
	a, fc, _, ctx, _, writes := ready(t)
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	if !strings.Contains(sentText(fc.take()), "I need your OK") {
		t.Fatal("no approval prompt")
	}
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", store.LevelRead)
	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if writes.Load() != 0 {
		t.Fatal("approved action ran after the level was lowered to read")
	}
	// And with the level left alone, approving runs it once.
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", "")
	a.Engine.HandleIncoming(ctx, dm("hal, turn on the light"))
	a.Engine.HandleIncoming(ctx, dm("approve"))
	if writes.Load() != 1 {
		t.Fatalf("approved action ran %d times", writes.Load())
	}
}

// The heartbeat (scheduler tick) hands a due job to a task run; the result
// reaches the user, and the spend is tagged heartbeat, unlike a chat.
func TestHeartbeatToTaskHandoff(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	daytime(t, a, owner.ID)
	j, err := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "morning", Schedule: "0 8 * * *", Prompt: "Say good morning.", Kind: store.JobCron})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(25 * time.Hour)
	a.Scheduler.Now = func() time.Time { return later }
	a.Scheduler.Tick(ctx)
	a.Scheduler.Wait()
	runs, _ := a.DB.JobRuns(ctx, j.ID, 5)
	if len(runs) == 0 || runs[0].Status != "ok" {
		t.Fatalf("job didn't run: %+v", runs)
	}
	if out := fc.take(); len(out) == 0 || out[len(out)-1].persona != "Juni" {
		t.Fatalf("job result not delivered: %+v", out)
	}
	a.Engine.HandleIncoming(ctx, dm("how are you?"))
	rows, _ := a.DB.UsageSummary(ctx, time.Now().Add(-time.Hour), "origin", "purpose")
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Keys[0]+"/"+r.Keys[1]] = true
	}
	if !got["heartbeat/job"] || !got["task/chat"] || got["task/job"] || got["heartbeat/chat"] {
		t.Fatalf("usage tags: %v", got)
	}
}

// At the spend cap, heartbeats are skipped (not failed and reported).
func TestHeartbeatsPauseAtTheCap(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "morning", Schedule: "0 8 * * *", Prompt: "Say good morning.", Kind: store.JobCron})
	a.Settings.Set(ctx, "budget.monthly_usd", "0.001")
	before := fm.calls
	later := time.Now().Add(25 * time.Hour)
	a.Scheduler.Now = func() time.Time { return later }
	a.Scheduler.Tick(ctx)
	a.Scheduler.Wait()
	if fm.calls != before {
		t.Fatal("a paused heartbeat called the model")
	}
	runs, _ := a.DB.JobRuns(ctx, j.ID, 5)
	if len(runs) != 1 || !strings.HasPrefix(runs[0].Error, "skipped: the monthly spend cap") {
		t.Fatalf("skip not recorded: %+v", runs)
	}
	if items, _ := a.DB.OpenInbox(ctx, owner.ID); len(items) != 0 {
		t.Fatalf("a deliberate pause was dead-lettered: %+v", items)
	}
	if strings.Contains(sentText(fc.take()), "didn't work") {
		t.Fatal("a pause was reported as a failure")
	}
}

// Failed work is never dropped: every failure lands in the inbox (repeats
// bump one item) even when the chat report is held back as a duplicate,
// and a result that can't be delivered is kept there too.
func TestFailuresAreDeadLettered(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	daytime(t, a, owner.ID)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "news", Schedule: "0 8 * * *", Prompt: "FAILME fetch the news.", Kind: store.JobCron})
	fm.mu.Lock()
	fm.failOn = "FAILME"
	fm.mu.Unlock()
	when := time.Now()
	reports := 0
	for day := 1; day <= 2; day++ {
		now := when.Add(time.Duration(day) * 25 * time.Hour)
		a.Scheduler.Now = func() time.Time { return now }
		a.Scheduler.Tick(ctx)
		a.Scheduler.Wait()
		reports += strings.Count(sentText(fc.take()), "didn't work")
	}
	if reports != 1 {
		t.Fatalf("failure reported in chat %d times, want once (6h de-duplication)", reports)
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxFailed || items[0].Ref != fmt.Sprintf("job:%d", j.ID) || items[0].Count != 2 {
		t.Fatalf("inbox: %+v", items)
	}

	// Juni can show it.
	a.Engine.HandleIncoming(ctx, dm("what needs me?"))
	var listed string
	for _, r := range fm.reqs {
		if last := r.Messages[len(r.Messages)-1]; last.Role == "tool" {
			listed = last.Content
		}
	}
	if !strings.Contains(listed, "Job “news” failed") || !strings.Contains(listed, "(×2)") {
		t.Fatalf("needs_you listed: %q", listed)
	}
	fc.take()

	// Undeliverable output is kept, not lost.
	fm.mu.Lock()
	fm.failOn = ""
	fm.mu.Unlock()
	a.DB.ResolveInbox(ctx, items[0].ID, owner.ID)
	fc.mu.Lock()
	fc.failIf = func(string) bool { return true }
	fc.mu.Unlock()
	now := when.Add(3 * 25 * time.Hour)
	a.Scheduler.Now = func() time.Time { return now }
	a.Scheduler.Tick(ctx)
	a.Scheduler.Wait()
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	items, _ = a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxUndelivered || items[0].Detail == "" {
		t.Fatalf("undelivered result not kept: %+v", items)
	}
}

// A restart fails in-flight work cleanly: interrupted job runs and
// delegated tasks go to the inbox and the user is told, once.
func TestRestartFailsInFlightWorkCleanly(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "backup", Schedule: "0 3 * * *", Prompt: "Check the backup.", Kind: store.JobCron})
	a.DB.StartJobRun(ctx, j.ID, time.Now())
	a.DB.StartTask(ctx, store.Task{UserID: owner.ID, From: "juniper", Character: "hal", Prompt: "check every light"})

	// What App.Run does at startup.
	interrupted := a.Engine.RecoverInterrupted(ctx)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Scheduler.Run(rctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := a.DB.JobRuns(ctx, j.ID, 1)
		if len(runs) == 1 && runs[0].Status == "error" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	a.Engine.ReportInterrupted(ctx, interrupted)

	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	kinds := map[string]string{}
	for _, it := range items {
		kinds[it.Ref] = it.Kind
	}
	if kinds[fmt.Sprintf("job:%d", j.ID)] != store.InboxInterrupted || kinds["task:1"] != store.InboxInterrupted {
		t.Fatalf("inbox after restart: %+v", items)
	}
	out := sentText(fc.take())
	if !strings.Contains(out, "Thicket restarted while I was working") || !strings.Contains(out, "backup") {
		t.Fatalf("user not told:\n%s", out)
	}
	// A second start finds nothing left over.
	if again := a.Engine.RecoverInterrupted(ctx); len(again) != 0 {
		t.Fatalf("interrupted tasks reported twice: %+v", again)
	}
}

// Memory: write (remember), read (list_memories), forget.
func TestMemoryRememberListForget(t *testing.T) {
	a, fc, fm, ctx, _, _ := ready(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.Engine.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	mems, _ := a.DB.AllMemories(ctx, owner.ID)
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "Biscuit") || !mems[0].Locked {
		t.Fatalf("remember: %+v", mems)
	}
	a.Engine.HandleIncoming(ctx, dm("what do you remember about me?"))
	var listed string
	for _, r := range fm.reqs {
		if last := r.Messages[len(r.Messages)-1]; last.Role == "tool" {
			listed = last.Content
		}
	}
	if !strings.Contains(listed, fmt.Sprintf("#%d", mems[0].ID)) || !strings.Contains(listed, "Biscuit") {
		t.Fatalf("list_memories: %q", listed)
	}
	a.Engine.HandleIncoming(ctx, dm(fmt.Sprintf("forget memory %d", mems[0].ID)))
	fc.take()
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 0 {
		t.Fatalf("forget left: %+v", mems)
	}
}

// Juni ships with five voice tests; a replay in voice passes and a drifted
// one is caught, with the never-say list checked in code.
func TestVoiceTests(t *testing.T) {
	a, _, fm, ctx, _, _ := ready(t)
	tests, err := character.LoadVoiceTests(a.Chars.Dir(), "juniper")
	if err != nil || len(tests) != 5 {
		t.Fatalf("Juni's voice tests: %d %v", len(tests), err)
	}
	for _, vt := range tests {
		if vt.Reference == "" || vt.Notes == "" {
			t.Fatalf("%s is incomplete", vt.Name)
		}
	}
	juni, _ := a.Chars.Get("juniper")
	vt := character.VoiceTest{Name: "t", Prompt: "voice: hello", Reference: "Hey you! All quiet in the garden."}

	fm.mu.Lock()
	fm.voice = "Hello! Quiet day in the garden so far."
	fm.mu.Unlock()
	reply, err := a.Engine.VoiceReply(ctx, juni, vt.Prompt)
	if err != nil || reply != "Hello! Quiet day in the garden so far." {
		t.Fatalf("replay: %q %v", reply, err)
	}
	if v, err := a.Engine.JudgeVoice(ctx, juni, vt, reply); err != nil || v.Drifted() {
		t.Fatalf("in-voice reply flagged: %+v %v", v, err)
	}

	fm.mu.Lock()
	fm.judgeDrift = true
	fm.mu.Unlock()
	if v, _ := a.Engine.JudgeVoice(ctx, juni, vt, reply); !v.Drifted() || v.Failed()[0] != "tone" {
		t.Fatalf("judge drift not reported: %+v", v)
	}
	fm.mu.Lock()
	fm.judgeDrift = false
	fm.mu.Unlock()
	v, _ := a.Engine.JudgeVoice(ctx, juni, vt, "Certainly! As an AI language model I'm happy to help.")
	if v.InRole {
		t.Fatalf("never-say phrases passed: %+v", v)
	}
	rows, _ := a.DB.UsageSummary(ctx, time.Now().Add(-time.Hour), "purpose")
	found := false
	for _, r := range rows {
		found = found || r.Keys[0] == "voice-check"
	}
	if !found {
		t.Fatal("voice-check calls weren't metered")
	}
}

// /health reports each MCP server without failing on it.
func TestHealthListsIntegrations(t *testing.T) {
	a, _, _, ctx, _, _ := ready(t)
	c, ok := a.health(ctx)["mcp.ha"]
	if !ok || !c.OK || !c.Optional || !strings.Contains(c.Detail, "2 tools") {
		t.Fatalf("mcp.ha health: %+v", c)
	}
}
