package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/app"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

func TestCostsReport(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, u := range []store.Usage{
		{Character: "juniper", Origin: llm.OriginTask, Purpose: "chat", Model: "anthropic/claude-sonnet-5.5", PromptTokens: 12000, CompletionTokens: 800, CostUSD: 0.5},
		{Character: "juniper", Origin: llm.OriginHeartbeat, Purpose: "job", Model: "anthropic/claude-sonnet-5.5", CostUSD: 0.25},
		{Character: "pip", Origin: llm.OriginHeartbeat, Purpose: "proactive", Model: "anthropic/claude-haiku-5.5", CostUSD: 1},
	} {
		db.RecordUsage(ctx, u)
	}
	st := settings.New(db, &config.Config{MonthlyBudgetUSD: 2})
	var out bytes.Buffer
	if err := printCosts(ctx, db, st, costsSince(time.Now(), false, 0), false, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"$1.7500 over 3 calls", "12k tokens in", "of the $2.00 cap (88%): over 80%",
		"pip  $1.0000  $0.0000  $1.0000", "juniper  $0.2500  $0.5000  $0.7500", "anthropic/claude-haiku-5.5"} {
		if !strings.Contains(strings.Join(strings.Fields(s), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if since := costsSince(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), true, 0); since != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("--month: %v", since)
	}
}

func TestOnlyModelKeyMissing(t *testing.T) {
	for body, want := range map[string]bool{
		`{"checks":{"database":{"ok":true},"model_key":{"ok":false}}}`:                true,
		`{"checks":{"model_key":{"ok":false},"mcp.ha":{"ok":false,"optional":true}}}`: true,
		`{"checks":{"model_key":{"ok":false},"database":{"ok":false}}}`:               false,
		`{"checks":{"database":{"ok":false}}}`:                                        false,
		`not json`:                                                                    false,
	} {
		if got := onlyModelKeyMissing(body); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}

// scripted replies to voice-test prompts, and a judge that flags one.
type voiceModel struct{}

func (voiceModel) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	sys := req.Messages[0].Content
	last := req.Messages[len(req.Messages)-1].Content
	out := "Hey you! All quiet in the garden."
	switch {
	case strings.HasPrefix(sys, "You check whether a character's new reply"):
		out = `{"tone": true, "length": true, "catchphrases": true, "in_role": true, "note": ""}`
		if _, reply, _ := strings.Cut(last, "New reply"); strings.Contains(reply, "Certainly") {
			out = `{"tone": false, "length": true, "catchphrases": true, "in_role": true, "note": "help-desk tone"}`
		}
	case strings.Contains(last, "drift please"):
		out = "Certainly! How may I assist you today?"
	}
	return &llm.Response{Model: "fake", Message: llm.Message{Role: "assistant", Content: out}}, nil
}

func TestVoiceCheckCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), ModelURL: config.DefaultModelURL,
		OpenRouterKey: "sk-or-v1-" + strings.Repeat("ab", 32), Model: "test/main", CheapModel: "test/cheap", LogLevel: "error", IdleAfter: time.Hour, HTTPAddr: "127.0.0.1:0"}
	ctx := context.Background()
	a, err := app.New(ctx, cfg, "test", app.Options{Model: voiceModel{}, NoHTTP: true, LogOut: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	juni, _ := a.Chars.Get("juniper")

	// The five bundled references replay in voice.
	var out bytes.Buffer
	if err := runVoiceCheck(ctx, a, juni, false, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "5 of 5 in voice") {
		t.Fatalf("output:\n%s", out.String())
	}

	// A new test without a reference is skipped, then recorded.
	vdir := character.VoiceTestDir(a.Chars.Dir(), "juniper")
	os.WriteFile(filepath.Join(vdir, "06-new.yaml"), []byte("prompt: what's new?\n"), 0o644)
	out.Reset()
	runVoiceCheck(ctx, a, juni, false, &out)
	if !strings.Contains(out.String(), "SKIP   06-new") {
		t.Fatalf("no-reference test not skipped:\n%s", out.String())
	}
	out.Reset()
	if err := runVoiceCheck(ctx, a, juni, true, &out); err != nil || !strings.Contains(out.String(), "SAVED  06-new") {
		t.Fatalf("record: %v\n%s", err, out.String())
	}
	tests, _ := character.LoadVoiceTests(a.Chars.Dir(), "juniper")
	if tests[5].Reference != "Hey you! All quiet in the garden." {
		t.Fatalf("recorded reference: %q", tests[5].Reference)
	}

	// Drift is flagged and fails the command.
	os.WriteFile(filepath.Join(vdir, "07-drift.yaml"), []byte("prompt: drift please\nreference: Ooh, something's sprouting.\n"), 0o644)
	out.Reset()
	err = runVoiceCheck(ctx, a, juni, false, &out)
	if err == nil || !strings.Contains(out.String(), "DRIFT  07-drift: tone, in role. used a banned phrase") {
		t.Fatalf("drift not flagged: %v\n%s", err, out.String())
	}
}
