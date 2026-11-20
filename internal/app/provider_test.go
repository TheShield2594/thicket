package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/secrets"
)

func TestHostedProviderOnboarding(t *testing.T) {
	a, fc, _, ctx := setup(t)
	e := a.Engine
	var askedWith []string
	e.ListModels = func(_ context.Context, base, key string) ([]string, error) {
		askedWith = append(askedWith, base+" "+key)
		if key != "gsk_goodgoodgoodgoodgoodgood1" {
			return nil, errors.New("401 invalid key")
		}
		return []string{"whisper-large-v3", "llama-3.1-8b-instant", "openai/gpt-oss-120b"}, nil
	}

	e.HandleIncoming(ctx, dm("hi"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Groq") || !strings.Contains(out[0].text, "OpenCode Go") {
		t.Fatalf("greeting should list the providers: %+v", out)
	}
	// Naming a provider without a key says where to get one.
	e.HandleIncoming(ctx, dm("I have a Nous Portal subscription"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "portal.nousresearch.com") {
		t.Fatalf("provider hint: %+v", out)
	}
	// A plain sk- key with no name isn't sent anywhere.
	e.HandleIncoming(ctx, dm("sk-0123456789abcdef0123456789abcdef"))
	fc.take()
	if len(askedWith) != 0 {
		t.Fatalf("an unnamed key was tried: %v", askedWith)
	}

	in := dm("gsk_badbadbadbadbadbadbadbad12")
	in.MessageID = "k1"
	e.HandleIncoming(ctx, in)
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "didn't accept") {
		t.Fatalf("bad key: %+v", out)
	}
	if e.ModelReady(ctx) || a.Secrets.Has(ctx, secrets.ModelAPIKey) {
		t.Fatal("a refused key was stored")
	}

	in = dm("gsk_goodgoodgoodgoodgoodgood1")
	in.MessageID = "k2"
	e.HandleIncoming(ctx, in)
	out := fc.take()
	if len(out) < 2 || !strings.Contains(out[0].text, "**openai/gpt-oss-120b**") || !strings.Contains(out[0].text, "llama-3.1-8b-instant") ||
		!strings.Contains(out[0].text, "model_usd_per_mtok_in") {
		t.Fatalf("connect: %+v", out)
	}
	if len(fc.deleted) < 2 || fc.deleted[len(fc.deleted)-1] != "k2" {
		t.Fatalf("key message not deleted: %v", fc.deleted)
	}
	for k, want := range map[string]string{"model_url": "https://api.groq.com/openai/v1", "model": "openai/gpt-oss-120b",
		"cheap_model": "llama-3.1-8b-instant", "fallback_model": "openai/gpt-oss-120b"} {
		if got := a.Settings.String(ctx, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !e.ModelReady(ctx) {
		t.Fatal("not ready after connecting")
	}
	if h := a.health(ctx); !h["model_key"].OK || h["model_key"].Detail != "Groq" {
		t.Fatalf("health: %+v", h["model_key"])
	}
}

func TestNamedProviderKey(t *testing.T) {
	a, fc, _, ctx := setup(t)
	var got string
	a.Engine.ListModels = func(_ context.Context, base, key string) ([]string, error) {
		got = base + " " + key
		return []string{"glm-4.6", "kimi-k2"}, nil
	}
	a.Engine.HandleIncoming(ctx, dm("opencode go sk-0123456789abcdef0123456789"))
	out := fc.take()
	if got != "https://opencode.ai/zen/go/v1 sk-0123456789abcdef0123456789" {
		t.Fatalf("asked %q", got)
	}
	if len(out) == 0 || !strings.Contains(out[0].text, "**kimi-k2**") || strings.Contains(out[0].text, "model_usd_per_mtok") {
		t.Fatalf("subscription connect: %+v", out)
	}
}
