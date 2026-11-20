package llm

import "testing"

func TestProviderFor(t *testing.T) {
	for url, want := range map[string]string{
		"https://opencode.ai/zen/v1":                              "opencode-zen",
		"https://opencode.ai/zen/go/v1":                           "opencode-go",
		"https://api.z.ai/api/coding/paas/v4":                     "zai-coding",
		"https://api.z.ai/api/paas/v4/":                           "zai",
		"https://inference.nousresearch.com/v1":                   "nous",
		"https://inference-api.nousresearch.com/v1":               "nous",
		"https://api.groq.com/openai/v1":                          "groq",
		"https://generativelanguage.googleapis.com/v1beta/openai": "gemini",
		"https://openrouter.ai/api/v1":                            "",
		"http://192.168.1.20:11434/v1":                            "",
		"not a url":                                               "",
	} {
		p, ok := ProviderFor(url)
		if got := map[bool]string{true: p.ID}[ok]; got != want {
			t.Errorf("ProviderFor(%q) = %q, want %q", url, got, want)
		}
	}
	if !IsHosted("https://openrouter.ai/api/v1") || !IsHosted("https://api.openai.com/v1") || IsHosted("http://ollama:11434/v1") {
		t.Fatal("IsHosted")
	}
	for _, p := range Providers {
		if q, ok := ProviderFor(p.BaseURL); !ok || q.ID != p.ID {
			t.Errorf("%s's own base URL maps to %q", p.ID, q.ID)
		}
		if p.KeysURL == "" || p.Name == "" || len(p.Names) == 0 {
			t.Errorf("%s is missing a name or keys URL", p.ID)
		}
	}
}

func TestProviderFromChat(t *testing.T) {
	if p, k, ok := ProviderByKey("here: gsk_abcdefghijklmnopqrstuvwxyz123"); !ok || p.ID != "groq" || k != "gsk_abcdefghijklmnopqrstuvwxyz123" {
		t.Fatalf("groq key: %v %q %v", p.ID, k, ok)
	}
	if _, _, ok := ProviderByKey("sk-abcdefghijklmnopqrstuvwxyz"); ok {
		t.Fatal("a plain sk- key has no single owner")
	}
	if _, _, ok := ProviderByKey("sk-or-v1-abcdefghijklmnopqrstuvwxyz"); ok {
		t.Fatal("OpenRouter keys are not a catalog provider's")
	}
	for text, want := range map[string]string{
		"opencode go sk-123":      "opencode-go",
		"OpenCode Zen key":        "opencode-zen",
		"use the GLM coding plan": "zai-coding",
		"nous research":           "nous",
		"deepseek":                "deepseek",
		"let's do this together":  "",
		"the hfoo thing":          "",
		"together.ai sk-123":      "together",
	} {
		p, ok := ProviderInText(text)
		if got := map[bool]string{true: p.ID}[ok]; got != want {
			t.Errorf("ProviderInText(%q) = %q, want %q", text, got, want)
		}
	}
	p, _ := ProviderByID("deepseek")
	if k := KeyInText(p, "deepseek sk-0123456789abcdef0123456789"); k != "sk-0123456789abcdef0123456789" {
		t.Fatalf("key = %q", k)
	}
}

func TestPickModels(t *testing.T) {
	p, _ := ProviderByID("groq")
	main, cheap := p.PickModels([]string{"whisper-large-v3", "llama-3.1-8b-instant", "openai/gpt-oss-120b", "openai/gpt-oss-20b"})
	if main != "openai/gpt-oss-120b" || cheap != "openai/gpt-oss-20b" {
		t.Fatalf("got %q, %q", main, cheap)
	}
	main, cheap = p.PickModels([]string{"text-embedding", "something-new"})
	if main != "something-new" || cheap != "something-new" {
		t.Fatalf("no preference matched: %q, %q", main, cheap)
	}
	if main, _ := p.PickModels(nil); main != "" {
		t.Fatal("no models, no pick")
	}
}
