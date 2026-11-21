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

// A hosted provider's key never reaches the owner's own server, also
// with OpenRouter in between, and also when the key is in the environment.
func TestProviderKeyStaysWithItsProvider(t *testing.T) {
	a, _, _, ctx := setup(t)
	e := a.Engine
	owner, err := a.DB.CreateUser(ctx, "web", "1", "Sam", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	const groqKey = "gsk_goodgoodgoodgoodgoodgood1"
	var localGot []string
	e.ListModels = func(_ context.Context, base, key string) ([]string, error) {
		if strings.Contains(base, "groq.com") {
			return []string{"openai/gpt-oss-120b"}, nil
		}
		localGot = append(localGot, key)
		return []string{"qwen2.5"}, nil
	}
	for _, step := range [][3]string{{"groq", groqKey, ""}, {"openrouter", "sk-or-v1-abcdefabcdefabcdefabcdef", ""}, {"local", "", "10.0.0.5:11434"}} {
		if _, err := e.AdminConnectProvider(ctx, owner, step[0], step[1], step[2]); err != nil {
			t.Fatalf("%s: %v", step[0], err)
		}
	}
	if a.Secrets.Has(ctx, secrets.ModelAPIKey) {
		t.Fatal("groq's key is still the model key on a local server")
	}
	if _, err := e.AdminModels(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range localGot {
		if k != "" {
			t.Fatalf("the local server was sent a key: %q", k)
		}
	}
	// Groq's key is kept for switching back.
	if _, err := e.AdminConnectProvider(ctx, owner, "groq", "", ""); err != nil {
		t.Fatal(err)
	}
	if k, _ := a.Secrets.Get(ctx, secrets.ModelAPIKey); k != groqKey {
		t.Fatal("groq's key wasn't kept")
	}

	// A key in the environment can't be removed from here, so a switch
	// that would send it elsewhere is refused.
	e.Cfg.ModelAPIKey, e.Cfg.ModelURL = groqKey, "https://api.groq.com/openai/v1"
	a.Secrets.SetEnvFallback(secrets.ModelAPIKey, groqKey)
	localGot = nil
	for _, to := range [][3]string{{"local", "", "10.0.0.5:11434"}, {"deepseek", "sk-deepseekdeepseekdeepseek01", ""}} {
		if _, err := e.AdminConnectProvider(ctx, owner, to[0], to[1], to[2]); err == nil || !strings.Contains(err.Error(), "THICKET_MODEL_API_KEY") {
			t.Fatalf("switch to %s with groq's key in the environment: %v", to[0], err)
		}
	}
	if len(localGot) != 0 || !strings.Contains(a.Settings.String(ctx, "model_url"), "groq.com") {
		t.Fatalf("the refused switch still happened: %q", localGot)
	}
}

// First-run setup with a hosted provider's key in the environment: a
// local address sent in chat is not tried, so the key isn't sent to it.
func TestChatLocalServerWithProviderKeyInEnv(t *testing.T) {
	a, fc, _, ctx := setup(t)
	e := a.Engine
	const groqKey = "gsk_goodgoodgoodgoodgoodgood1"
	e.Cfg.ModelAPIKey, e.Cfg.ModelURL = groqKey, "https://api.groq.com/openai/v1"
	a.Secrets.SetEnvFallback(secrets.ModelAPIKey, groqKey)
	var asked []string
	e.ListModels = func(_ context.Context, base, key string) ([]string, error) {
		asked = append(asked, base+" "+key)
		return []string{"qwen2.5"}, nil
	}
	e.HandleIncoming(ctx, dm("http://10.0.0.5:11434"))
	out := fc.take()
	if len(asked) != 0 {
		t.Fatalf("the server was asked, with the key: %q", asked)
	}
	if len(out) != 1 || !strings.Contains(out[0].text, "THICKET_MODEL_API_KEY") || strings.Contains(out[0].text, groqKey) {
		t.Fatalf("reply: %+v", out)
	}
}
