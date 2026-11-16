package app

// Smoke test: the whole system wired together, with a fake Discord and a
// scripted fake model. It walks first-run onboarding, memory, approvals,
// handoffs, memory review and health.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
)

type sent struct {
	chat, persona, text string
}

type fakeChannel struct {
	name    string // "" means discord
	mu      sync.Mutex
	sent    []sent
	deleted []string
	// failIf makes Send fail for matching text (simulates a dropped post).
	failIf func(text string) bool
}

func (f *fakeChannel) Name() string {
	if f.name == "" {
		return "discord"
	}
	return f.name
}
func (f *fakeChannel) Run(ctx context.Context, h channel.Handler) error {
	<-ctx.Done()
	return nil
}
func (f *fakeChannel) Send(_ context.Context, chatID string, p channel.Persona, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIf != nil && f.failIf(text) {
		return fmt.Errorf("send failed")
	}
	f.sent = append(f.sent, sent{chatID, p.Name, text})
	return nil
}
func (f *fakeChannel) SendDirect(ctx context.Context, userID string, p channel.Persona, text string) (string, error) {
	return "dm-" + userID, f.Send(ctx, "dm-"+userID, p, text)
}
func (f *fakeChannel) Delete(_ context.Context, chatID, msgID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, msgID)
	return nil
}
func (f *fakeChannel) Typing(context.Context, string) {}
func (f *fakeChannel) Connected() bool                { return true }

func (f *fakeChannel) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

// fakeModel answers based on who is speaking and what was asked.
type fakeModel struct {
	mu     sync.Mutex
	calls  int
	onChat func() // runs on every call (e.g. to move the clock)
	reqs   []llm.Request
	// rejectImages answers requests with images the way OpenRouter does
	// for a model without vision.
	rejectImages bool
	// pdfText is what the fake PDF parser "finds"; parseFail makes the
	// parse-only call fail; annotateTurn returns annotations on chat
	// replies, as non-native PDF engines do.
	pdfText      string
	parseFail    bool
	annotateTurn bool
	parseCalls   int
	// rejectTools answers requests with tools the way Ollama does for a
	// model without tool support.
	rejectTools bool
	// compactFail makes summarising fail; contextLimit makes requests
	// whose messages exceed it fail like a full context window.
	compactFail  bool
	contextLimit int
	compactions  int
}

// annotations builds OpenRouter-style parse annotations for the files in
// the newest user message.
func annotations(msgs []llm.Message, text string) json.RawMessage {
	var anns []any
	for _, m := range msgs {
		for _, f := range m.Files {
			anns = append(anns, map[string]any{"type": "file", "file": map[string]any{"hash": "h-" + f.Name, "name": f.Name,
				"content": []any{map[string]any{"type": "text", "text": text}}}})
		}
	}
	if anns == nil {
		return nil
	}
	b, _ := json.Marshal(anns)
	return b
}

func toolCall(name string, args any) *llm.Response {
	b, _ := json.Marshal(args)
	return &llm.Response{Model: "fake", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20, Cost: 0.001},
		Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: name, Arguments: string(b)}}}}}
}

func text(s string) *llm.Response {
	return &llm.Response{Model: "fake", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20, Cost: 0.001}, Message: llm.Message{Role: "assistant", Content: s}}
}

func (m *fakeModel) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	m.calls++
	hook := m.onChat
	m.reqs = append(m.reqs, req)
	reject := m.rejectImages
	pdfText, parseFail, annotate := m.pdfText, m.parseFail, m.annotateTurn
	if req.Messages[0].Content == "Reply with OK." {
		m.parseCalls++
	}
	m.mu.Unlock()
	if pdfText == "" {
		pdfText = "Quarterly revenue rose 12%."
	}
	if req.Messages[0].Content == "Reply with OK." {
		if parseFail {
			return nil, &llm.APIError{Status: 502, Message: "parser down"}
		}
		r := text("O")
		r.Message.Annotations = annotations(req.Messages, pdfText)
		return r, nil
	}
	if annotate && llm.HasFiles(req.Messages) {
		r := text("Read it.")
		r.Message.Annotations = annotations(req.Messages, pdfText+" (from the turn)")
		return r, nil
	}
	m.mu.Lock()
	noTools := m.rejectTools
	compactFail, limit := m.compactFail, m.contextLimit
	if strings.HasPrefix(req.Messages[0].Content, "You keep the running summary") {
		m.compactions++
	}
	m.mu.Unlock()
	if strings.HasPrefix(req.Messages[0].Content, "You keep the running summary") {
		if compactFail {
			return nil, &llm.APIError{Status: 502, Message: "upstream down"}
		}
		return text("SUMMARY-NOTES: the user listed topics, starting with topic-01."), nil
	}
	if limit > 0 {
		n := 0
		for _, msg := range req.Messages[1:] { // the system prompt is fixed
			n += len(msg.Content)
		}
		if n > limit {
			return nil, &llm.APIError{Status: 400, Message: "This model's maximum context length is 8192 tokens"}
		}
	}
	if noTools && req.Tools != nil {
		return nil, &llm.APIError{Status: 400, Message: "registry.ollama.ai/library/tiny:1b does not support tools"}
	}
	if reject && llm.HasMedia(req.Messages) {
		return nil, &llm.APIError{Status: 404, Message: "No endpoints found that support image input"}
	}
	if hook != nil {
		hook()
	}
	sys := req.Messages[0].Content
	last := req.Messages[len(req.Messages)-1]
	switch {
	case strings.Contains(sys, "You check whether a chat reply"):
		return text(`{"in_character": true}`), nil
	case strings.Contains(sys, "You maintain long-term memory"):
		return text(`{"ops":[{"op":"add","scope":"shared","kind":"person","content":"Has a sister called Maya."}]}`), nil
	case strings.HasPrefix(sys, "You are Pip"):
		return text("Pip here! I'll sort your calendar."), nil
	}
	// Juniper. "spin" keeps calling tools for as long as they're offered,
	// to prove the loop cap (not the fake) ends the turn.
	if lastUserText(req.Messages) == "spin forever" {
		if req.Tools != nil {
			return toolCall("list_memories", map[string]any{}), nil
		}
		return text("Okay, stopping there."), nil
	}
	if last.Role == "tool" {
		if strings.HasPrefix(last.Content, "PENDING") {
			return text("I'll jot that down as soon as you give me the nod."), nil
		}
		if strings.Contains(last.Content, "will pick this up") {
			return text("Passing you to Pip."), nil
		}
		return text("Let me plant that thought. Done!"), nil
	}
	u := strings.ToLower(last.Content)
	switch {
	case strings.Contains(u, "remember my dog"):
		return toolCall("remember", map[string]any{"content": "Has a dog named Biscuit.", "scope": "shared", "kind": "fact", "user_asked": true}), nil
	case strings.HasPrefix(u, "save skill "):
		steps := "1. Weather\n2. Calendar"
		base := 0
		switch strings.TrimPrefix(u, "save skill ") {
		case "with key":
			steps = "1. Log in with ghp_" + strings.Repeat("k", 36)
		case "v2":
			steps, base = "1. Weather\n2. Calendar\n3. One nice thing", 1
		case "stale":
			steps, base = "1. Something else", 1
		}
		return toolCall("save_skill", map[string]any{"id": "briefing", "name": "Morning briefing",
			"description": "When putting together the morning summary", "steps": steps, "base_version": base}), nil
	case strings.Contains(u, "use the briefing skill"):
		return toolCall("read_skill", map[string]any{"id": "briefing"}), nil
	case strings.HasPrefix(u, "restore briefing to "):
		var v int
		fmt.Sscanf(u, "restore briefing to %d", &v)
		return toolCall("manage_skill", map[string]any{"id": "briefing", "action": "restore", "version": v}), nil
	case strings.Contains(u, "link my other app"):
		return toolCall("link_account", map[string]any{}), nil
	case strings.Contains(u, "webhook for ci"):
		return toolCall("create_webhook", map[string]any{"name": "CI watch", "task": "Tell me what broke."}), nil
	case strings.HasPrefix(u, "new url for job "):
		var id int
		fmt.Sscanf(u, "new url for job %d", &id)
		return toolCall("update_job", map[string]any{"job_id": id, "action": "new_url"}), nil
	case strings.HasPrefix(u, "pause job "):
		var id int
		fmt.Sscanf(u, "pause job %d", &id)
		return toolCall("update_job", map[string]any{"job_id": id, "action": "pause"}), nil
	case strings.Contains(u, "calendar"):
		return toolCall("handoff", map[string]any{"to": "pip", "message": "help with the calendar"}), nil
	}
	return text("Hi! I'm Juni. Lovely to meet you."), nil
}

// lastUserText is the newest message the user actually typed.
func lastUserText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && !strings.HasPrefix(msgs[i].Content, "[") {
			return msgs[i].Content
		}
	}
	return ""
}

const pip = `---
id: pip
name: Pip
role: Keeps the calendar and makes sure nothing is late.
personality:
  traits: [punctual, upbeat]
voice:
  style: [brisk and cheerful]
tools: [schedule]
---
Pip is small and tidy.
`

func setup(t *testing.T) (*App, *fakeChannel, *fakeModel, context.Context) {
	t.Helper()
	fc := &fakeChannel{}
	a, fm, ctx := setupWith(t, fc)
	return a, fc, fm, ctx
}

func setupWith(t *testing.T, chans ...*fakeChannel) (*App, *fakeModel, context.Context) {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("THICKET_DATA_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("THICKET_DATA_DIR") })
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.CharactersDir = filepath.Join(dir, "characters") // never touch a developer's real files
	fm := &fakeModel{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, "test", Options{
		Model:    fm,
		KeyCheck: func(context.Context, string) error { return nil },
		Channels: func(*slog.Logger) []channel.Channel {
			var out []channel.Channel
			for _, c := range chans {
				out = append(out, c)
			}
			return out
		},
		LogOut: io.Discard,
		NoHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, fm, ctx
}

func dm(text string) channel.Incoming {
	return channel.Incoming{Channel: "discord", ChatID: "dm-owner", MessageID: fmt.Sprint(time.Now().UnixNano()), UserID: "owner", UserName: "Sam", Text: text, IsDM: true}
}

func TestSmoke(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine

	// 1. First contact, no model key: scripted greeting, owner created.
	e.HandleIncoming(ctx, dm("hello?"))
	out := fc.take()
	if len(out) != 1 || out[0].persona != "Juni" || !strings.Contains(out[0].text, "openrouter.ai/keys") {
		t.Fatalf("bootstrap greeting: %+v", out)
	}
	owner, err := a.DB.UserByExternal(ctx, "discord", "owner")
	if err != nil || owner.Role != "owner" {
		t.Fatalf("owner not created: %+v %v", owner, err)
	}

	// 2. Strangers are turned away and not created.
	e.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "dm-x", UserID: "stranger", Text: "hi", IsDM: true})
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "private") {
		t.Fatalf("stranger reply: %+v", out)
	}
	if _, err := a.DB.UserByExternal(ctx, "discord", "stranger"); err == nil {
		t.Fatal("stranger should not get an account")
	}

	// 3. Owner pastes their key: captured, never stored as a message.
	key := "sk-or-v1-" + strings.Repeat("ab12", 16)
	e.HandleIncoming(ctx, dm(key))
	out = fc.take()
	if len(out) != 2 || !strings.Contains(out[0].text, "it works") || !strings.Contains(out[1].text, "Juni") {
		t.Fatalf("key capture: %+v", out)
	}
	if !a.Secrets.Has(ctx, "openrouter_api_key") {
		t.Fatal("key not stored")
	}
	assertNoLeak(t, a, key)

	// 4. Memory: the user asks Juni to remember something.
	e.HandleIncoming(ctx, dm("Juni, please remember my dog is called Biscuit"))
	out = fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "plant that thought") {
		t.Fatalf("remember reply: %+v", out)
	}
	mems, _ := a.DB.AllMemories(ctx, owner.ID)
	if len(mems) != 1 || !mems[0].Locked || mems[0].Content != "Has a dog named Biscuit." {
		t.Fatalf("memory not saved/locked: %+v", mems)
	}

	// 5. Approvals: require approval for remember, then approve.
	if err := a.DB.SetToolPolicy(ctx, "remember", "always"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.ApplyMemoryChanges(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	e.HandleIncoming(ctx, dm("remember my dog is a beagle too"))
	out = fc.take()
	if len(out) != 2 || !strings.Contains(out[1].text, "approve") || !strings.Contains(out[1].text, "remember") {
		t.Fatalf("approval prompt: %+v", out)
	}
	e.HandleIncoming(ctx, dm("approve"))
	out = fc.take()
	if len(out) != 1 {
		t.Fatalf("after approval: %+v", out)
	}
	runs, _ := a.DB.RecentActivity(ctx, 50, "info")
	if !hasActivity(runs, "approved") || !hasActivity(runs, "ran approved tool remember") {
		t.Fatal("approval not logged")
	}

	// 6. Handoff to another sprig (added as a file, picked up on reload).
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	e.HandleIncoming(ctx, dm("can someone help with my calendar?"))
	out = fc.take()
	if len(out) != 2 || out[0].persona != "Juni" || out[1].persona != "Pip" || !strings.Contains(out[1].text, "Pip here") {
		t.Fatalf("handoff: %+v", out)
	}
	// Pip stays the active sprig in this chat.
	e.HandleIncoming(ctx, dm("thanks!"))
	if out = fc.take(); len(out) != 1 || out[0].persona != "Pip" {
		t.Fatalf("sticky character: %+v", out)
	}

	// 7. Runaway tool loops are capped.
	before := fm.calls
	e.HandleIncoming(ctx, dm("Juni, spin forever"))
	if out = fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "stopping") {
		t.Fatalf("loop cap: %+v", out)
	}
	// At most 6 model calls for the turn, plus one personality check.
	if n := fm.calls - before; n < 6 || n > 7 {
		t.Fatalf("loop cap: %d model calls", n)
	}

	// 7b. Addressing a sprig works on multi-line messages too.
	e.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "general", UserID: "owner", Text: "Pip, summarize these:\nfirst\nsecond"})
	if out = fc.take(); len(out) != 1 || out[0].persona != "Pip" {
		t.Fatalf("multiline address in a server channel: %+v", out)
	}
	e.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "general", UserID: "owner", Text: "hey Juni\nhow are you?"})
	if out = fc.take(); len(out) != 1 || out[0].persona != "Juni" {
		t.Fatalf("multiline 'hey Juni': %+v", out)
	}

	// 8. A secret pasted in a server channel is deleted, never processed.
	e.HandleIncoming(ctx, channel.Incoming{Channel: "discord", ChatID: "general", MessageID: "m1", UserID: "owner", Text: "Juni, token ghp_" + strings.Repeat("a", 36), Mentioned: true})
	if out = fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "deleted") {
		t.Fatalf("guild secret: %+v", out)
	}
	if len(fc.deleted) == 0 || fc.deleted[len(fc.deleted)-1] != "m1" {
		t.Fatal("secret message not deleted")
	}
	assertNoLeak(t, a, "ghp_"+strings.Repeat("a", 36))

	// 9. Memory review of the idle conversation.
	a.DB.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	a.Reviewer.ReviewPending(ctx)
	mems, _ = a.DB.AllMemories(ctx, owner.ID)
	found := false
	for _, m := range mems {
		found = found || m.Content == "Has a sister called Maya."
	}
	if !found {
		t.Fatalf("review did not add memory: %+v", mems)
	}
	reviews, _ := a.DB.ListExtractionRuns(ctx, 5)
	if len(reviews) == 0 || reviews[0].Status != "ok" {
		t.Fatalf("extraction runs: %+v", reviews)
	}

	// 10. Usage was recorded and health is green.
	spent, _ := a.DB.SpendSince(ctx, time.Now().Add(-time.Hour), "")
	if spent <= 0 {
		t.Fatal("usage not recorded")
	}
	a.Scheduler.Tick(ctx)
	for name, c := range a.health(ctx) {
		if !c.OK {
			t.Errorf("health %s not ok: %s", name, c.Detail)
		}
	}
}

func TestBudgetCapStopsCalls(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("cd34", 16)))
	fc.take()
	if err := a.Settings.Set(ctx, "budget.daily_usd", "0.0001"); err != nil {
		t.Fatal(err)
	}
	before := fm.calls
	e.HandleIncoming(ctx, dm("hello again"))
	out := fc.take()
	if fm.calls != before {
		t.Fatalf("model called past the cap (%d -> %d)", before, fm.calls)
	}
	// The user gets the in-character refusal; the owner gets the alert.
	var gotReply, gotAlert bool
	for _, s := range out {
		gotReply = gotReply || strings.Contains(s.text, "spending cap")
		gotAlert = gotAlert || strings.Contains(s.text, "daily spend cap")
	}
	if !gotReply || !gotAlert {
		t.Fatalf("budget messages: %+v", out)
	}
}

func assertNoLeak(t *testing.T, a *App, secret string) {
	t.Helper()
	for _, q := range []string{"SELECT content FROM messages", "SELECT summary || detail FROM activity", "SELECT content FROM memories"} {
		rows, err := a.DB.DB().Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			if strings.Contains(s, secret) {
				rows.Close()
				t.Fatalf("secret leaked via %q: %s", q, s)
			}
		}
		rows.Close()
	}
}

func hasActivity(acts interface{}, substr string) bool {
	b, _ := json.Marshal(acts)
	return strings.Contains(string(b), substr)
}

func TestUndeliveredApprovalCannotBeApproved(t *testing.T) {
	a, fc, _, ctx := setup(t)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ef56", 16)))
	fc.take()
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.SetToolPolicy(ctx, "remember", "always")

	// The approval prompt fails to send.
	fc.mu.Lock()
	fc.failIf = func(s string) bool { return strings.Contains(s, "I need your OK") }
	fc.mu.Unlock()
	e.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	fc.take()
	if _, err := a.DB.LatestPendingApproval(ctx, owner.ID, "discord:dm-owner", time.Hour); err == nil {
		t.Fatal("an approval the user never saw was registered")
	}
	// A later bare "ok" must not run anything.
	e.HandleIncoming(ctx, dm("ok"))
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 0 {
		t.Fatalf("unseen approval was executed: %+v", mems)
	}
}

func TestCheckInDeferredByQuietHoursIsRetried(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ab78", 16)))
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.MarkOnboarded(ctx, owner.ID)
	// The owner last wrote 5 days ago (Juni checks in after 3).
	a.DB.SetClock(func() time.Time { return time.Now().Add(-5 * 24 * time.Hour) })
	conv, _ := a.DB.OpenConversation(ctx, owner.ID, "discord:dm-owner", time.Minute)
	a.DB.AddMessage(ctx, conv.ID, "user", "", "talk soon")
	a.DB.SetClock(time.Now)
	fc.take()

	// Juni's quiet hours are 22:00-08:00 (owner is on UTC). Generation
	// starts at 21:59 and finishes at 22:01.
	day := time.Now().UTC().Truncate(24 * time.Hour)
	var mu sync.Mutex
	clock := day.Add(21*time.Hour + 59*time.Minute)
	e.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	fm.mu.Lock()
	fm.onChat = func() { mu.Lock(); clock = day.Add(22*time.Hour + time.Minute); mu.Unlock() }
	fm.mu.Unlock()

	e.CheckIns(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("check-in sent during quiet hours: %+v", out)
	}

	// Next morning it goes out, exactly once.
	fm.mu.Lock()
	fm.onChat = nil
	fm.mu.Unlock()
	mu.Lock()
	clock = day.Add(24*time.Hour + 9*time.Hour)
	mu.Unlock()
	e.CheckIns(ctx)
	if out := fc.take(); len(out) != 1 {
		t.Fatalf("deferred check-in not retried: %+v", out)
	}
	e.CheckIns(ctx)
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("check-in sent twice in one quiet spell: %+v", out)
	}
}
