package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/setup"
	"github.com/theshield2594/thicket/internal/store"
)

// fakeServices answers like OpenRouter, Discord, Telegram and a local
// model server, accepting only the "good" credentials.
func fakeServices(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/key" && r.Header.Get("Authorization") == "Bearer sk-or-good":
			w.Write([]byte(`{"data":{}}`))
		case r.URL.Path == "/users/@me" && r.Header.Get("Authorization") == "Bot discord-good":
			w.Write([]byte(`{"username":"thicketbot"}`))
		case r.URL.Path == "/bottg-good/getMe":
			w.Write([]byte(`{"ok":true,"result":{"username":"thicket_bot"}}`))
		case r.URL.Path == "/v1/models":
			w.Write([]byte(`{"data":[{"id":"llama3.1"},{"id":"qwen2.5"}]}`))
		default:
			http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSetup(t *testing.T, api string) (*setup.Setup, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), HTTPAddr: ":8080",
		ModelURL: config.DefaultModelURL, Model: config.DefaultModel, DailyBudgetUSD: 2, MonthlyBudgetUSD: 30}
	db, err := store.Open(context.Background(), filepath.Join(dir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _, err := secrets.ResolveKey("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(db, key, secrets.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	return &setup.Setup{DB: db, Secrets: sec, Settings: settings.New(db, cfg), Verify: true,
		OpenRouterAPI: api, DiscordAPI: api, TelegramAPI: api}, cfg
}

func TestInitAsksChecksAndStores(t *testing.T) {
	srv := fakeServices(t)
	s, cfg := newSetup(t, srv.URL)
	ctx := context.Background()

	// A wrong key is refused and asked again; a wrong Discord token is
	// left unset with Enter; the rest go straight in.
	answers := strings.Join([]string{
		"1", "sk-or-bad", "sk-or-good", // model
		"discord-bad", "", // discord: refused, then skipped
		"tg-good",         // telegram
		"Mars/Olympus",    // timezone: refused
		"America/Chicago", // timezone
		"1.50", "",        // daily cap, monthly kept
		"discord:not-a-number", "telegram:4242", // owner
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := initAsk(ctx, s, cfg, strings.NewReader(answers), &out); err != nil {
		t.Fatal(err)
	}
	if err := initFinish(ctx, s, cfg, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, secret := range []string{"sk-or-good", "sk-or-bad", "discord-bad", "tg-good"} {
		if strings.Contains(text, secret) {
			t.Errorf("the output shows a secret (%s):\n%s", secret, text)
		}
	}
	for _, want := range []string{"OpenRouter didn't accept that key", "Discord didn't accept that token", "bot @thicket_bot",
		"needs an IANA timezone", "telegram:4242", "/login#", "$1.50 a day, $30.00 a month"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	st, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OpenRouterKey || st.DiscordToken || !st.TelegramToken || st.Timezone != "America/Chicago" || st.Owner != "telegram:4242" || !st.ModelReady() {
		t.Fatalf("state after init: %+v", st)
	}
	if v, _ := s.Secrets.Get(ctx, secrets.OpenRouterKey); v != "sk-or-good" {
		t.Fatalf("stored key = %q", v)
	}
	// Nothing secret is in the settings table or any plain file: the key
	// is only in the encrypted store.
	for _, name := range []string{"model", "model_url", "timezone", "budget.daily_usd"} {
		if v, _, _ := s.DB.Setting(ctx, name); strings.Contains(v, "sk-or") || strings.Contains(v, "tg-good") {
			t.Errorf("setting %s holds a secret", name)
		}
	}
	// The owner starts in the household's timezone.
	if u, err := s.DB.UserByExternal(ctx, "telegram", "4242"); err != nil || u.Role != store.RoleOwner || u.Timezone != "America/Chicago" {
		t.Fatalf("owner: %+v %v", u, err)
	}

	// Running it again with only Enter changes nothing and shows what's
	// set, with secrets as "stored".
	out.Reset()
	if err := initAsk(ctx, s, cfg, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	again := out.String()
	for _, want := range []string{"OpenRouter, key stored", "Telegram (optional). Bot token: stored", "Now: America/Chicago", "The owner is telegram:4242"} {
		if !strings.Contains(again, want) {
			t.Errorf("second run lacks %q:\n%s", want, again)
		}
	}
	if st2, _ := s.State(ctx); st2 != st {
		t.Fatalf("a run with no answers changed things: %+v -> %+v", st, st2)
	}
}

func TestInitLocalServer(t *testing.T) {
	srv := fakeServices(t)
	s, cfg := newSetup(t, srv.URL)
	ctx := context.Background()
	// A dead address is refused; the live one lists its models and the
	// second is chosen.
	answers := strings.Join([]string{"2", "http://127.0.0.1:1/v1", "", srv.URL + "/v1", "", "qwen2.5"}, "\n") + "\n"
	var out bytes.Buffer
	if err := initAsk(ctx, s, cfg, strings.NewReader(answers), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "It has: llama3.1, qwen2.5") || !strings.Contains(out.String(), "no model server answered") {
		t.Fatalf("output:\n%s", out.String())
	}
	st, _ := s.State(ctx)
	if !st.Local || st.Model != "qwen2.5" || st.ModelURL != srv.URL+"/v1" || !st.ModelReady() {
		t.Fatalf("state: %+v", st)
	}
	if got := s.Settings.String(ctx, "cheap_model"); got != "qwen2.5" {
		t.Fatalf("cheap_model = %q: a local install has only its own models", got)
	}
	// Back to OpenRouter: the address and the local model id are dropped.
	if _, err := s.OpenRouterKey(ctx, "sk-or-good"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(ctx); st.Local || st.Model != config.DefaultModel {
		t.Fatalf("after switching to OpenRouter: %+v", st)
	}
}

func TestInitFromEnv(t *testing.T) {
	srv := fakeServices(t)
	s, cfg := newSetup(t, srv.URL)
	ctx := context.Background()
	var out bytes.Buffer

	// Nothing set: says so, stores nothing, succeeds.
	t.Setenv("TZ", "")
	if err := initFromEnv(ctx, s, cfg, &out); err != nil || !strings.Contains(out.String(), "Nothing to store") {
		t.Fatalf("empty environment: %v\n%s", err, out.String())
	}

	// One bad value fails the command by name; the good ones are stored.
	cfg.OpenRouterKey, cfg.DiscordToken, cfg.TelegramToken, cfg.TelegramOwnerID = "sk-or-good", "discord-bad", "tg-good", "77"
	t.Setenv("TZ", "Europe/London")
	t.Setenv("THICKET_DAILY_BUDGET_USD", "5")
	out.Reset()
	err := initFromEnv(ctx, s, cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "DISCORD_TOKEN") || strings.Contains(err.Error(), "TELEGRAM") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "discord-bad") || strings.Contains(out.String(), "sk-or-good") {
		t.Fatalf("output shows a secret:\n%s", out.String())
	}
	st, _ := s.State(ctx)
	if !st.OpenRouterKey || st.DiscordToken || !st.TelegramToken || st.Timezone != "Europe/London" || st.Owner != "telegram:77" || st.Daily != "$5.00" {
		t.Fatalf("state: %+v", st)
	}

	// Offline: --no-verify stores without asking the services.
	s.Verify = false
	cfg.DiscordToken = "discord-unchecked"
	if err := initFromEnv(ctx, s, cfg, &out); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(ctx); !st.DiscordToken {
		t.Fatal("--no-verify should store the token")
	}
}
