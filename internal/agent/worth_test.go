package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// presenceLLM writes the sprig's message (reply) and answers the cheap
// worth-saying check (worth), counting how often it was asked.
type presenceLLM struct {
	mu     sync.Mutex
	reply  string
	worth  func(draft string) bool
	judged int
	// tool, when set, is called once (when offered) before the reply.
	tool string
}

func (f *presenceLLM) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tool != "" && len(req.Tools) > 0 && req.Messages[len(req.Messages)-1].Role != "tool" {
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: f.tool, Arguments: "{}"}}}}}, nil
	}
	if req.Messages[0].Content == worthRubric {
		f.judged++
		_, draft, _ := strings.Cut(req.Messages[len(req.Messages)-1].Content, "The message it's about to send:\n")
		ok := f.worth == nil || f.worth(draft)
		out, _ := json.Marshal(map[string]any{"worth": ok, "why": "it only says there's nothing on"})
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: string(out)}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.reply}}, nil
}

// sentChannel is a chat app that records what it was asked to send.
type sentChannel struct {
	mu   sync.Mutex
	sent []string
}

func (c *sentChannel) Name() string                                                { return "web" }
func (c *sentChannel) Run(ctx context.Context, _ channel.Handler) error            { <-ctx.Done(); return nil }
func (c *sentChannel) Send(context.Context, string, channel.Persona, string) error { return nil }
func (c *sentChannel) Delete(context.Context, string, string) error                { return nil }
func (c *sentChannel) Typing(context.Context, string)                              {}
func (c *sentChannel) Connected() bool                                             { return true }
func (c *sentChannel) SendDirect(_ context.Context, userID string, _ channel.Persona, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, text)
	return userID, nil
}

func (c *sentChannel) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

type presenceFixture struct {
	e    *Engine
	db   *store.Store
	llm  *presenceLLM
	ch   *sentChannel
	user *store.User
	char *character.Character
	now  time.Time
}

func newPresenceFixture(t *testing.T) *presenceFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _ := secrets.NewKey()
	sec, err := secrets.New(db, key, secrets.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir, IdleAfter: time.Hour}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &presenceFixture{db: db, llm: &presenceLLM{}, ch: &sentChannel{}, now: time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)}
	db.SetClock(func() time.Time { return f.now })
	charDir := filepath.Join(dir, "characters")
	if err := os.MkdirAll(charDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.e = &Engine{Cfg: cfg, DB: db, LLM: f.llm, Settings: settings.New(db, cfg), Secrets: sec, Log: log,
		Chars: character.NewRegistry(charDir, db, log), Now: func() time.Time { return f.now }}
	f.e.AddChannel(f.ch)
	// Only the worth-saying check may ask the cheap model.
	if err := f.e.Settings.Set(ctx, "consistency_check", "false"); err != nil {
		t.Fatal(err)
	}
	pip := &character.Character{ID: "pip", Name: "Pip", Role: "Keeps the day on track.",
		Personality: character.Personality{Traits: []string{"cheerful"}}, Voice: character.Voice{Style: []string{"short"}},
		Initiative: character.Initiative{CheckIns: true, FollowUps: true, DailyMessages: 10}}
	if _, err := f.e.Chars.Write(ctx, pip); err != nil {
		t.Fatal(err)
	}
	f.char, _ = f.e.Chars.Get("pip")
	if f.user, err = db.CreateUser(ctx, "web", "u1", "Sam", store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	return f
}

// heldBack reports whether the activity log says something was held back
// for why.
func (f *presenceFixture) heldBack(t *testing.T, why string) bool {
	t.Helper()
	acts, err := f.db.UserActivity(context.Background(), f.user.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if strings.Contains(a.Summary, "held back") && strings.Contains(a.Summary, why) {
			return true
		}
	}
	return false
}

// A morning brief on a day with no events, nothing due and nothing new
// sends nothing, and the run is empty; with something on the calendar it
// goes out.
func TestMorningBriefWithNothingNewSendsNothing(t *testing.T) {
	f := newPresenceFixture(t)
	ctx := context.Background()
	// The brief reads the calendar (and finds nothing on it).
	f.char.Tools = []string{character.ScopeCalendar}
	if _, err := f.e.Chars.Write(ctx, f.char); err != nil {
		t.Fatal(err)
	}
	f.char, _ = f.e.Chars.Get("pip")
	f.llm.tool = "calendar"
	job, err := f.db.CreateJob(ctx, store.Job{UserID: f.user.ID, Character: "pip", Name: "Morning brief", Schedule: "0 7 * * *",
		Prompt: "Send the morning briefing.", Kind: store.JobCron, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Yesterday's brief went out.
	today := f.now
	f.now = today.Add(-24 * time.Hour)
	if err := f.e.sendProactive(ctx, f.user, f.char, "Morning! The dentist is at 09:30 today."); err != nil {
		t.Fatal(err)
	}
	f.now = today

	f.llm.reply = "Good morning! Nothing on the calendar today and nothing due, so enjoy a quiet one."
	res := f.e.RunJob(ctx, *job, 0)
	if res.Err != nil || res.Status != "empty" || !strings.Contains(res.Held, "nothing new") {
		t.Fatalf("an empty day's brief: %+v", res)
	}
	if f.llm.judged != 0 {
		t.Fatal("\"nothing changed\" must be decided without a model")
	}
	f.e.JobFinished(ctx, *job, res, time.Time{})
	if n := f.ch.count(); n != 1 {
		t.Fatalf("the brief was sent anyway (%d messages)", n)
	}
	if !f.heldBack(t, "nothing new") {
		t.Fatal("the activity log doesn't say what was held back")
	}

	// Something on the calendar today: worth a brief.
	if _, err := f.db.AddSprigEvent(ctx, store.SprigEvent{UserID: f.user.ID, Character: "pip", Kind: "reminder", Title: "Bins out", StartsAt: today.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.llm.reply = "Morning! The bins go out at 10:00."
	res = f.e.RunJob(ctx, *job, 0)
	if res.Status != "ok" {
		t.Fatalf("a brief with news: %+v", res)
	}
	if f.llm.judged != 1 {
		t.Fatalf("the cheap check ran %d times", f.llm.judged)
	}
	f.e.JobFinished(ctx, *job, res, time.Time{})
	if n := f.ch.count(); n != 2 {
		t.Fatalf("the brief with news wasn't sent (%d messages)", n)
	}
}

// A check-in that says what yesterday's said is suppressed in code, even
// when something changed; a different one goes out.
func TestRepeatedCheckInIsSuppressed(t *testing.T) {
	f := newPresenceFixture(t)
	ctx := context.Background()
	today := f.now
	f.now = today.Add(-24 * time.Hour)
	f.llm.reply = "Hey Sam! Just checking in — hope the week's going well. Still planning that trip to the coast?"
	if err := f.e.proactive(ctx, f.user, f.char, "check in", proactiveDraft{what: "a check-in"}); err != nil {
		t.Fatal(err)
	}
	if f.ch.count() != 1 {
		t.Fatal("the first check-in wasn't sent")
	}
	f.now = today
	// Something new, so only the repeat holds the next one back.
	if _, err := f.db.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add",
		Memory: store.Memory{UserID: f.user.ID, Kind: "fact", Content: "Sam is training for a 10k", Source: "auto"}}}); err != nil {
		t.Fatal(err)
	}
	judged := f.llm.judged
	f.llm.reply = "Hey Sam, just checking in: hope the week is going well! Still planning that trip to the coast?"
	err := f.e.proactive(ctx, f.user, f.char, "check in", proactiveDraft{what: "a check-in"})
	if !errors.Is(err, errNotWorth) {
		t.Fatalf("a repeated check-in: %v", err)
	}
	if f.ch.count() != 1 {
		t.Fatal("the repeated check-in was sent")
	}
	if f.llm.judged != judged {
		t.Fatal("a repeat must be caught without a model")
	}
	if !f.heldBack(t, "repeats") {
		t.Fatal("the activity log doesn't say what was held back")
	}

	f.llm.reply = "Hi Sam! How's the 10k training going?"
	if err := f.e.proactive(ctx, f.user, f.char, "check in", proactiveDraft{what: "a check-in"}); err != nil {
		t.Fatal(err)
	}
	if f.ch.count() != 2 {
		t.Fatal("a new check-in wasn't sent")
	}
}

// When there is news, the cheap model still holds back filler.
func TestFillerIsHeldBackByTheCheapCheck(t *testing.T) {
	f := newPresenceFixture(t)
	ctx := context.Background()
	f.llm.worth = func(draft string) bool { return !strings.Contains(draft, "nothing new") }
	f.llm.reply = "Just a note: nothing new today!"
	err := f.e.proactive(ctx, f.user, f.char, "check in", proactiveDraft{what: "a check-in"})
	if !errors.Is(err, errNotWorth) || f.ch.count() != 0 {
		t.Fatalf("filler: %v, %d sent", err, f.ch.count())
	}
	if !f.heldBack(t, "not worth saying") {
		t.Fatal("the activity log doesn't say what was held back")
	}
}

// A routine that reads nothing (a daily lesson, a poem) has its own news:
// the cheap model judges it, even on a day when nothing else changed.
func TestStandaloneRoutineIsJudgedNotHeld(t *testing.T) {
	f := newPresenceFixture(t)
	ctx := context.Background()
	job, err := f.db.CreateJob(ctx, store.Job{UserID: f.user.ID, Character: "pip", Name: "Word of the day", Schedule: "0 7 * * *",
		Prompt: "Teach one new Spanish word with an example sentence.", Kind: store.JobCron, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	today := f.now
	f.now = today.Add(-24 * time.Hour)
	if err := f.e.sendProactive(ctx, f.user, f.char, "Today's word: mariposa (butterfly). Una mariposa azul."); err != nil {
		t.Fatal(err)
	}
	f.now = today

	f.llm.reply = "Today's word: ventana (window). Abre la ventana, por favor."
	res := f.e.RunJob(ctx, *job, 0)
	if res.Status != "ok" || f.llm.judged != 1 {
		t.Fatalf("a standalone routine: %+v (judged %d)", res, f.llm.judged)
	}
	// The judge still decides, and a rerun is still caught in code.
	f.llm.worth = func(string) bool { return false }
	f.llm.reply = "Today's word: perro (dog). El perro duerme."
	if res := f.e.RunJob(ctx, *job, 0); res.Status != "empty" || !strings.Contains(res.Held, "not worth saying") {
		t.Fatalf("judged not worth it: %+v", res)
	}
	f.llm.reply = "Today's word: mariposa (butterfly). Una mariposa azul!"
	judged := f.llm.judged
	if res := f.e.RunJob(ctx, *job, 0); res.Status != "empty" || !strings.Contains(res.Held, "repeats") || f.llm.judged != judged {
		t.Fatalf("a rerun: %+v", res)
	}
}

// Held messages that say the same thing (two runs during quiet hours, or
// one that repeats what went out since) go out once.
func TestHeldRepeatsAreDroppedOnFlush(t *testing.T) {
	f := newPresenceFixture(t)
	ctx := context.Background()
	if err := f.e.sendProactive(ctx, f.user, f.char, "The bins go out tonight at 19:00."); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"Heads up: the bins go out tonight at 19:00!", // repeats what was sent
		"Dentist moved to 11/12 at 09:30.",
		"Dentist moved to 11/12, at 09:30!", // repeats the one before
		"Dentist moved to 12/11 at 09:30.",  // a different date
	} {
		if _, err := f.db.HoldMessage(ctx, store.HeldMessage{UserID: f.user.ID, Character: "pip", Ref: "job:9",
			Title: "job", Reason: store.HeldQuiet, Text: text, CreatedAt: f.now}); err != nil {
			t.Fatal(err)
		}
	}
	f.e.FlushHeld(ctx)
	f.ch.mu.Lock()
	sent := append([]string(nil), f.ch.sent...)
	f.ch.mu.Unlock()
	if len(sent) != 3 || !strings.Contains(sent[1], "11/12") || !strings.Contains(sent[2], "12/11") {
		t.Fatalf("sent: %q", sent)
	}
	if left, _ := f.db.HeldMessages(ctx, 0, ""); len(left) != 0 {
		t.Fatalf("still held: %+v", left)
	}
	if !f.heldBack(t, "repeats") {
		t.Fatal("the activity log doesn't say what was dropped")
	}
	if f.llm.judged != 0 {
		t.Fatal("the held repeat check must not use a model")
	}
}

func TestRepeats(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Hey Sam! Just checking in — hope the week's going well.", "Hey Sam, just checking in: hope the week is going well!", true},
		{"Dentist at 09:30 today, bins out tonight.", "Dentist at 10:30 today, bins out tonight.", false}, // the time changed
		{"Morning! Dentist at 09:30.", "Rain later: take a coat to football practice.", false},
		{"", "anything", false},
		{"Your appointment is on 11/12 at 09:30.", "Your appointment is on 12/11 at 09:30.", false}, // the date changed
	} {
		if got := repeats(c.a, c.b); got != c.want {
			t.Errorf("repeats(%q, %q) = %v (similarity %.2f)", c.a, c.b, got, similarity(c.a, c.b))
		}
	}
}
