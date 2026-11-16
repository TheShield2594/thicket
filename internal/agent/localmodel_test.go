package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

func TestLocalCandidates(t *testing.T) {
	for in, want := range map[string][]string{
		"http://192.168.1.20:11434":      {"http://192.168.1.20:11434/v1", "http://192.168.1.20:11434"},
		"it's at http://box:1234/v1/ ok": {"http://box:1234/v1"},
		"10.0.0.5:8000":                  {"http://10.0.0.5:8000/v1", "http://10.0.0.5:8000"},
		"https://llm.example.com/api":    {"https://llm.example.com/api", "https://llm.example.com/api/v1"},
		"I use Ollama":                   {"http://host.docker.internal:11434/v1", "http://ollama:11434/v1", "http://localhost:11434/v1"},
		"lm studio":                      {"http://host.docker.internal:1234/v1", "http://localhost:1234/v1"},
		"hello!":                         nil,
		"https://openrouter.ai/api/v1":   nil,
		"meet at 10:30":                  nil,
	} {
		if got := localCandidates(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n got %v\nwant %v", in, got, want)
		}
	}
}

func TestFitHistory(t *testing.T) {
	msg := func(n int) store.Message { return store.Message{Role: "user", Content: strings.Repeat("x", n)} }
	h := []store.Message{msg(1000), msg(1000), msg(1000), msg(50)}
	got, dropped := fitHistory(h, 2000, 0) // sizes count 40 characters of framing each
	if len(got) != 2 || !dropped || got[1].Content != h[3].Content {
		t.Fatalf("kept %d, dropped=%v", len(got), dropped)
	}
	// The newest message always goes in, even alone over budget.
	got, dropped = fitHistory([]store.Message{msg(500), msg(9000)}, 2000, 0)
	if len(got) != 1 || !dropped {
		t.Fatalf("newest over budget: kept %d", len(got))
	}
	if got, dropped := fitHistory(h, 100000, 0); len(got) != 4 || dropped {
		t.Fatal("everything fits")
	}
	// The message the turn answers stays, even if a notice came after it.
	h[1].ID = 42
	if got, _ := fitHistory(h, 200, 42); len(got) != 3 || got[0].ID != 42 {
		t.Fatalf("message being answered dropped: kept %d", len(got))
	}
}
