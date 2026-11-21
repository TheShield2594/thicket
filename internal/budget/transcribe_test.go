package budget

import (
	"context"
	"testing"

	"github.com/theshield2594/thicket/internal/llm"
)

type ears struct{ calls int }

func (e *ears) Transcribe(_ context.Context, req llm.TranscriptionRequest) (*llm.Transcript, error) {
	e.calls++
	return &llm.Transcript{Text: "hello", Model: req.Model}, nil
}

func TestTranscriptionIsRecordedFreeAndCapped(t *testing.T) {
	g, db, _ := guard(t, 0)
	e := &ears{}
	g.Ears = e
	ctx := llm.WithMeta(context.Background(), llm.Meta{Character: "pip", Purpose: "transcribe"})
	if tr, err := g.Transcribe(ctx, llm.TranscriptionRequest{URL: "http://voice:7071/v1", Model: "whisper-1"}); err != nil || tr.Text != "hello" {
		t.Fatalf("transcribe: %+v %v", tr, err)
	}
	var n int
	var cost float64
	db.DB().QueryRow("SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM usage WHERE purpose = 'transcribe' AND model = 'whisper-1'").Scan(&n, &cost)
	if n != 1 || cost != 0 {
		t.Fatalf("usage rows %d, cost %v", n, cost)
	}
	// Past a cap, nothing is transcribed (like every other call).
	if err := g.Settings.Set(ctx, "budget.monthly_usd", "0.5"); err != nil {
		t.Fatal(err)
	}
	if err := g.Settings.Set(ctx, "speech_usd_per_1k_chars", "1000"); err != nil {
		t.Fatal(err)
	}
	g.Voice = &speaker{hosted: true}
	if _, err := g.Speak(ctx, llm.SpeechRequest{Input: "spend it all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Transcribe(ctx, llm.TranscriptionRequest{URL: "http://voice:7071/v1"}); !llm.IsBudget(err) || e.calls != 1 {
		t.Fatalf("past the cap: %v (calls %d)", err, e.calls)
	}
	g.Ears = nil
	if _, err := g.Transcribe(ctx, llm.TranscriptionRequest{}); err == nil {
		t.Fatal("no transcriber")
	}
}
