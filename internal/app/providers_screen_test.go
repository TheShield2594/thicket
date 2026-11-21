package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// The Providers list on the Settings screen: a key is checked before it's
// stored, switching keeps the other providers' keys, and a hosted
// provider's key never goes to the owner's own server.
func TestProvidersScreen(t *testing.T) {
	a, _, _, ctx := setup(t)
	e := a.Engine
	owner, err := a.DB.CreateUser(ctx, "web", "1", "Sam", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	member, err := a.DB.CreateUser(ctx, "web", "2", "Kit", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	const groqKey, dsKey, orKey = "gsk_goodgoodgoodgoodgoodgood1", "sk-deepseekdeepseekdeepseek01", "sk-or-v1-abcdefabcdefabcdefabcdef"
	var localGot []string
	e.ListModels = func(_ context.Context, base, key string) ([]string, error) {
		switch {
		case strings.Contains(base, "groq.com") && key == groqKey:
			return []string{"whisper-large-v3", "llama-3.1-8b-instant", "openai/gpt-oss-120b"}, nil
		case strings.Contains(base, "deepseek.com") && key == dsKey:
			return []string{"deepseek-chat"}, nil
		case base == "http://10.0.0.5:11434/v1":
			localGot = append(localGot, key)
			return []string{"qwen2.5"}, nil
		}
		return nil, errors.New("401 invalid key")
	}
	status := func() map[string]agent.ProviderInfo {
		out := map[string]agent.ProviderInfo{}
		for _, p := range e.AdminProviders(ctx).([]agent.ProviderInfo) {
			out[p.ID] = p
		}
		return out
	}
	want := func(when string, states map[string]string) {
		t.Helper()
		got := status()
		for id, s := range states {
			if got[id].Status != s {
				t.Errorf("%s: %s is %q, want %q", when, id, got[id].Status, s)
			}
		}
	}

	want("at first", map[string]string{"openrouter": "none", "groq": "none", "local": "none"})
	if _, err := e.AdminConnectProvider(ctx, member, "groq", groqKey, ""); err == nil {
		t.Fatal("a member connected a provider")
	}
	if _, err := e.AdminConnectProvider(ctx, owner, "groq", "gsk_badbadbadbadbadbadbadbad12", ""); err == nil || strings.Contains(err.Error(), "gsk_bad") {
		t.Fatalf("a refused key: %v", err)
	}
	if a.Secrets.Has(ctx, secrets.ModelAPIKey) {
		t.Fatal("a refused key was stored")
	}
	if _, err := e.AdminConnectProvider(ctx, owner, "groq", "", ""); err == nil {
		t.Fatal("switched to a provider with no key")
	}

	if _, err := e.AdminConnectProvider(ctx, owner, "groq", groqKey, ""); err != nil {
		t.Fatal(err)
	}
	want("after groq", map[string]string{"groq": "in_use", "openrouter": "none"})
	if got := status()["groq"].Model; got != "openai/gpt-oss-120b" {
		t.Fatalf("groq model: %q", got)
	}
	if got := a.Settings.String(ctx, "cheap_model"); got != "llama-3.1-8b-instant" {
		t.Fatalf("cheap model: %q", got)
	}
	if models, err := e.AdminModels(ctx); err != nil || len(models) != 2 {
		t.Fatalf("models: %v %v", models, err)
	}

	// A second provider takes over; the first one's key is kept.
	if _, err := e.AdminConnectProvider(ctx, owner, "deepseek", dsKey, ""); err != nil {
		t.Fatal(err)
	}
	want("after deepseek", map[string]string{"deepseek": "in_use", "groq": "ready"})
	if k, _ := a.Secrets.Get(ctx, secrets.ModelAPIKey); k != dsKey {
		t.Fatal("the model key isn't deepseek's")
	}
	// Back to the first without pasting its key again.
	if _, err := e.AdminConnectProvider(ctx, owner, "groq", "", ""); err != nil {
		t.Fatal(err)
	}
	want("back on groq", map[string]string{"groq": "in_use", "deepseek": "ready"})
	if k, _ := a.Secrets.Get(ctx, secrets.ModelAPIKey); k != groqKey {
		t.Fatal("the model key isn't groq's")
	}

	// The owner's own server gets no provider's key.
	if _, err := e.AdminConnectProvider(ctx, owner, "local", "", "10.0.0.5:11434"); err != nil {
		t.Fatal(err)
	}
	want("on a local server", map[string]string{"local": "in_use", "groq": "ready", "deepseek": "ready"})
	if len(localGot) != 1 || localGot[0] != "" || a.Secrets.Has(ctx, secrets.ModelAPIKey) {
		t.Fatalf("a provider's key could reach the local server: %q", localGot)
	}
	if got := a.Settings.String(ctx, "model"); got != "qwen2.5" {
		t.Fatalf("local model: %q", got)
	}

	// OpenRouter: the default models come back.
	if _, err := e.AdminConnectProvider(ctx, owner, "openrouter", orKey, ""); err != nil {
		t.Fatal(err)
	}
	want("on OpenRouter", map[string]string{"openrouter": "in_use", "local": "none", "groq": "ready"})
	if got := a.Settings.String(ctx, "model"); got != config.DefaultModel {
		t.Fatalf("OpenRouter model: %q", got)
	}

	// Removing keys.
	if _, err := e.AdminDisconnectProvider(ctx, member, "groq"); err == nil {
		t.Fatal("a member removed a key")
	}
	if _, err := e.AdminDisconnectProvider(ctx, owner, "groq"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AdminDisconnectProvider(ctx, owner, "openrouter"); err != nil {
		t.Fatal(err)
	}
	want("after removing", map[string]string{"groq": "none", "openrouter": "none", "deepseek": "ready"})
	if e.ModelReady(ctx) {
		t.Fatal("ready with no key")
	}
}
