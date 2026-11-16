package agent

import (
	"reflect"
	"testing"
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
