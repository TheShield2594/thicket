package app

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/llm"
)

// moodJudge answers the personality check itself and passes everything
// else on.
type moodJudge struct {
	llm.Client
	drift atomic.Bool
	mood  atomic.Value // string
}

func (m *moodJudge) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if strings.Contains(req.Messages[0].Content, "You check whether a chat reply") {
		if m.drift.Load() {
			return text(`{"in_character": false, "issue": "sounds like a help desk", "mood": "pleased"}`), nil
		}
		mood, _ := m.mood.Load().(string)
		return text(`{"in_character": true, "mood": "` + mood + `"}`), nil
	}
	return m.Client.Chat(ctx, req)
}

// A drift rewrite leaves the sprig unsure, a reply that lands leaves it
// pleased, and both fade back to idle.
func TestReplyMoodsDriveTheAvatar(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ab12", 16)))
	fc.take()

	var mu sync.Mutex
	clock := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	judge := &moodJudge{Client: e.LLM}
	judge.mood.Store("pleased")
	e.LLM = judge
	fm.mu.Lock()
	fm.say = "Of course! Here's everything about the garden you asked for, all sorted and ready to go."
	fm.mu.Unlock()

	if st := e.SprigStates(ctx); st["juniper"] != "idle" {
		t.Fatalf("before: %v", st)
	}
	judge.drift.Store(true)
	e.HandleIncoming(ctx, dm("juni, how's the garden?"))
	if out := fc.take(); len(out) != 1 || out[0].persona != "Juni" {
		t.Fatalf("reply: %+v", out)
	}
	fm.mu.Lock()
	rewrote := false
	for _, r := range fm.reqs {
		last := r.Messages[len(r.Messages)-1].Content
		rewrote = rewrote || strings.Contains(last, "Your draft reply drifted out of character")
	}
	fm.mu.Unlock()
	if st := e.SprigStates(ctx); !rewrote || st["juniper"] != "unsure" {
		t.Fatalf("after a drift rewrite (rewritten: %v): %v", rewrote, st)
	}
	if svg, ok := a.Avatar(ctx, "juniper", ""); !ok || !strings.Contains(svg, `class="sprig is-unsure"`) {
		t.Fatalf("live avatar: %s", svg)
	}
	advance(4 * time.Minute)
	if st := e.SprigStates(ctx); st["juniper"] != "unsure" {
		t.Fatalf("faded too soon: %v", st)
	}
	advance(2 * time.Minute)
	if st := e.SprigStates(ctx); st["juniper"] != "idle" {
		t.Fatalf("after the mood fades: %v", st)
	}

	judge.drift.Store(false)
	e.HandleIncoming(ctx, dm("juni, how's the garden now?"))
	fc.take()
	if st := e.SprigStates(ctx); st["juniper"] != "pleased" {
		t.Fatalf("after a reply that landed: %v", st)
	}
	judge.mood.Store("unsure")
	e.HandleIncoming(ctx, dm("juni, can you water it for me?"))
	fc.take()
	if st := e.SprigStates(ctx); st["juniper"] != "unsure" {
		t.Fatalf("after an unsure reply: %v", st)
	}
	advance(6 * time.Minute)
	if st := e.SprigStates(ctx); st["juniper"] != "idle" {
		t.Fatalf("faded: %v", st)
	}
	// The variants are served on request too.
	for _, st := range []string{"pleased", "unsure"} {
		if svg, ok := a.Avatar(ctx, "juniper", st); !ok || !strings.Contains(svg, `class="sprig is-`+st+`"`) {
			t.Fatalf("%s variant: %s", st, svg)
		}
	}
}
