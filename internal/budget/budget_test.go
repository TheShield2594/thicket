package budget

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

type costly struct{ cost float64 }

func (c costly) Chat(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{Model: "m", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 2, Cost: c.cost}}, nil
}

func guard(t *testing.T, cost float64) (*Guard, *store.Store, *[]Alert) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := settings.New(db, &config.Config{DailyBudgetUSD: 0, MonthlyBudgetUSD: 10})
	var mu sync.Mutex
	alerts := &[]Alert{}
	g := &Guard{Next: costly{cost}, DB: db, Settings: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnAlert: func(_ context.Context, a Alert) { mu.Lock(); *alerts = append(*alerts, a); mu.Unlock() }}
	return g, db, alerts
}

func TestMonthlyCapWarnsAt80AndPausesAt100(t *testing.T) {
	g, db, alerts := guard(t, 3)
	ctx := llm.WithMeta(context.Background(), llm.Meta{Character: "juniper", Purpose: "chat"})
	for i := 0; i < 3; i++ { // $9 of $10
		if _, err := g.Chat(ctx, llm.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(*alerts) != 1 || (*alerts)[0].Level != "warn" {
		t.Fatalf("want one 80%% warning, got %+v", *alerts)
	}
	if why := g.HeartbeatPause(ctx, "juniper"); why != "" {
		t.Fatalf("heartbeats paused under the cap: %s", why)
	}
	if _, err := g.Chat(ctx, llm.Request{}); err != nil { // $12: this call was allowed (under cap before it)
		t.Fatal(err)
	}
	// Crossing the cap alerts at once, before any call is refused.
	if last := (*alerts)[len(*alerts)-1]; last.Level != "error" || !strings.HasSuffix(last.Key, ":100") {
		t.Fatalf("no alert when the cap was crossed: %+v", *alerts)
	}
	_, err := g.Chat(ctx, llm.Request{})
	if !llm.IsBudget(err) {
		t.Fatalf("over the cap the call must be refused: %v", err)
	}
	if why := g.HeartbeatPause(ctx, ""); why == "" {
		t.Fatal("heartbeats not paused at 100%")
	}
	last := (*alerts)[len(*alerts)-1]
	if last.Level != "error" {
		t.Fatalf("no 100%% alert: %+v", *alerts)
	}
	spent, _ := db.SpendSince(context.Background(), time.Now().Add(-time.Hour), "")
	if spent != 12 {
		t.Fatalf("spend recorded %v, want 12", spent)
	}
}

// Calls are tagged heartbeat or task by where they started, and a nested
// purpose (a consistency check inside a job) keeps its origin.
func TestUsageIsTaggedHeartbeatOrTask(t *testing.T) {
	g, db, _ := guard(t, 0.01)
	job := llm.WithMeta(context.Background(), llm.Meta{Character: "pip", JobID: 3, Purpose: "job"})
	chat := llm.WithMeta(context.Background(), llm.Meta{Character: "pip", Purpose: "chat"})
	for _, ctx := range []context.Context{job, llm.WithPurpose(job, "consistency"), chat, llm.WithPurpose(chat, "consistency")} {
		if _, err := g.Chat(ctx, llm.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.UsageSummary(context.Background(), time.Now().Add(-time.Hour), "origin", "purpose")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.Keys[0]+"/"+r.Keys[1]] = r.Calls
	}
	want := map[string]int{"heartbeat/job": 1, "heartbeat/consistency": 1, "task/chat": 1, "task/consistency": 1}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("usage tags: got %v, want %v", got, want)
		}
	}
}

func TestPromptMixIsRecordedAndNamedInTheWarning(t *testing.T) {
	g, db, alerts := guard(t, 3) // 10 tokens in, 2 out, $3 a call
	ctx := llm.WithMeta(context.Background(), llm.Meta{Character: "juniper", Purpose: "chat"})
	// 1,000 characters of prompt: 600 earlier conversation, 200 PDF text.
	req := llm.Request{Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", 1000)}},
		Mix: &llm.PromptMix{HistoryChars: 600, DocumentChars: 200}}
	for i := 0; i < 3; i++ { // $9 of $10: the 80% warning
		if _, err := g.Chat(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	mix, err := db.PromptMixSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Input is 10 of 10 + 4×2 weighted tokens: 10/18 of the cost; history
	// is 60% of that.
	if mix.Calls != 3 || mix.CostUSD != 9 || mix.HistoryTokens != 18 || mix.DocTokens != 6 {
		t.Fatalf("mix: %+v", mix)
	}
	if h := mix.HistoryShare(); h < 0.33 || h > 0.34 {
		t.Fatalf("history share %.3f, want 0.333", h)
	}
	if d := mix.DocShare(); d < 0.11 || d > 0.12 {
		t.Fatalf("document share %.3f, want 0.111", d)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0].Message, "history_tokens (now automatic)") ||
		!strings.Contains((*alerts)[0].Message, "document_chars (now 20000)") {
		t.Fatalf("80%% warning doesn't name the settings: %+v", *alerts)
	}
	// Calls that weren't measured (memory review, say) don't count.
	g.Chat(llm.WithMeta(context.Background(), llm.Meta{Purpose: "memory"}), llm.Request{})
	if m2, _ := db.PromptMixSince(ctx, time.Now().Add(-time.Hour)); m2.Calls != 3 {
		t.Fatalf("unmeasured call counted: %+v", m2)
	}
	if line := MixLine(mix); !strings.Contains(line, "33%") || !strings.Contains(line, "11%") {
		t.Fatalf("mix line: %s", line)
	}
}

func TestProviderPricesCountWhenCostIsNotReported(t *testing.T) {
	g, _, _ := guard(t, 0) // 10 prompt and 2 reply tokens, no reported cost
	ctx := context.Background()
	call := func() float64 {
		mctx, m := WithMeter(ctx)
		if _, err := g.Chat(mctx, llm.Request{}); err != nil {
			t.Fatal(err)
		}
		return m.USD()
	}
	set := func(k, v string) {
		if err := g.Settings.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	set("model_url", "https://api.groq.com/openai/v1")
	if got := call(); got != 0 {
		t.Fatalf("no prices set: %v", got)
	}
	set("model_usd_per_mtok_in", "100000")
	set("model_usd_per_mtok_out", "200000")
	if got := call(); got < 1.3999 || got > 1.4001 {
		t.Fatalf("priced: %v, want 1.40", got)
	}
	set("model_url", "https://opencode.ai/zen/go/v1") // a subscription
	if got := call(); got != 0 {
		t.Fatalf("subscription: %v", got)
	}
	set("model_url", "https://openrouter.ai/api/v1") // reports its own cost
	if got := call(); got != 0 {
		t.Fatalf("openrouter: %v", got)
	}
}
