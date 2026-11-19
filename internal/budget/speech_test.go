package budget

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/llm"
)

type speaker struct {
	hosted bool
	calls  int
}

func (s *speaker) Speak(_ context.Context, req llm.SpeechRequest) (*llm.Speech, error) {
	s.calls++
	return &llm.Speech{Audio: []byte("OggS"), Model: req.Model, Hosted: s.hosted}, nil
}

func TestSpeechIsMeteredAndCapped(t *testing.T) {
	g, db, _ := guard(t, 0)
	sp := &speaker{hosted: true}
	g.Voice = sp
	ctx := llm.WithMeta(context.Background(), llm.Meta{UserID: 0, Character: "pip", Purpose: "speech"})
	if err := g.Settings.Set(ctx, "speech_usd_per_1k_chars", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Speak(ctx, llm.SpeechRequest{Model: "openai/gpt-4o-mini-tts", Input: string(make([]rune, 500))}); err != nil {
		t.Fatal(err)
	}
	spent, err := db.SpendSince(ctx, time.Now().Add(-time.Hour), "pip")
	if err != nil || math.Abs(spent-1) > 1e-9 {
		t.Fatalf("500 characters at $2 per 1,000 should cost $1, recorded %v (%v)", spent, err)
	}
	// A local speech server is free.
	sp.hosted = false
	if _, err := g.Speak(ctx, llm.SpeechRequest{Input: "hello"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.SpendSince(ctx, time.Now().Add(-time.Hour), "pip"); again != spent {
		t.Fatalf("local speech was charged: %v", again-spent)
	}
	// At the cap, nothing is spoken.
	if err := g.Settings.Set(ctx, "budget.monthly_usd", "1"); err != nil {
		t.Fatal(err)
	}
	before := sp.calls
	if _, err := g.Speak(ctx, llm.SpeechRequest{Input: "hello"}); !llm.IsBudget(err) || sp.calls != before {
		t.Fatalf("speech past the cap: %v (calls %d -> %d)", err, before, sp.calls)
	}
}
