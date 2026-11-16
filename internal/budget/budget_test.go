package budget

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
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
