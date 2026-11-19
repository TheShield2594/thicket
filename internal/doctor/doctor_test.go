package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

func TestDoctor(t *testing.T) {
	const discordTok = "discord-token-123"
	const telegramTok = "123456:telegram-secret"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			if r.Header.Get("Authorization") != "Bearer local-key" {
				http.Error(w, "401", 401)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"qwen2.5"}]}`)
		case r.URL.Path == "/discord/users/@me":
			if r.Header.Get("Authorization") != "Bot "+discordTok {
				http.Error(w, "401", 401)
				return
			}
			fmt.Fprint(w, `{"username":"thicket-bot"}`)
		case strings.HasPrefix(r.URL.Path, "/telegram/"):
			http.Error(w, `{"ok":false}`, 401) // a revoked token
		case r.URL.Path == "/health":
			fmt.Fprint(w, `{"status":"degraded","version":"1.2","checks":{"database":{"ok":true},"channel.discord":{"ok":false},"scheduler":{"ok":true,"detail":"last tick now"}}}`)
		case r.URL.Path == "/mcp":
			b, _ := io.ReadAll(r.Body)
			var id int
			if _, err := fmt.Sscanf(between(string(b), `"id":`, ","), "%d", &id); err != nil {
				w.WriteHeader(202)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(string(b), `"initialize"`) {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, id)
			} else {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"a","inputSchema":{}},{"name":"b","inputSchema":{}}]}}`, id)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), ModelURL: fake.URL + "/v1",
		DiscordToken: discordTok, TelegramToken: telegramTok, HTTPAddr: ":0", ModelAPIKey: "local-key"}
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(dir, "thicket.db"))
	db.PutMCPServer(ctx, store.MCPServer{Name: "files", Transport: "http", Target: fake.URL + "/mcp", Enabled: true})
	db.PutMCPServer(ctx, store.MCPServer{Name: "gone", Transport: "http", Target: fake.URL + "/nope", Enabled: true})
	db.Close()

	d := &Doctor{Cfg: cfg, DiscordAPI: fake.URL + "/discord", TelegramAPI: fake.URL + "/telegram", ServerURL: fake.URL}
	got := map[string]Check{}
	var all strings.Builder
	for _, c := range d.Run(ctx) {
		got[c.Name] = c
		fmt.Fprintf(&all, "%s ok=%v opt=%v %s\n", c.Name, c.OK, c.Optional, c.Detail)
	}
	report := all.String()
	expect := func(name string, ok bool, detail string) {
		t.Helper()
		c, found := got[name]
		if !found || c.OK != ok || !strings.Contains(c.Detail, detail) {
			t.Errorf("%s: want ok=%v and %q, got %+v\n%s", name, ok, detail, c, report)
		}
	}
	expect("config", true, "")
	expect("database", true, "integrity ok")
	expect("secrets", true, "decrypt")
	expect("model server", true, "reachable")
	expect("discord token", true, "thicket-bot")
	expect("telegram token", false, "refused")
	expect("mcp.files", true, "2 tools")
	expect("mcp.gone", false, "not responding")
	expect("server", false, "failing: channel.discord")
	expect("live discord", false, "not connected")
	expect("live scheduler", true, "last tick")
	if !got["mcp.gone"].Optional {
		t.Error("an integration being down must not fail the doctor")
	}
	if strings.Contains(report, telegramTok) || strings.Contains(report, discordTok) {
		t.Fatalf("a token leaked into the report:\n%s", report)
	}
}

func TestDoctorWithoutServer(t *testing.T) {
	dir := t.TempDir()
	const tok = "999:unreachable-secret"
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), ModelURL: "http://127.0.0.1:1/v1", HTTPAddr: ":0", TelegramToken: tok}
	got := map[string]Check{}
	for _, c := range (&Doctor{Cfg: cfg, ServerURL: "http://127.0.0.1:1", TelegramAPI: "http://127.0.0.1:1"}).Run(context.Background()) {
		got[c.Name] = c
	}
	// A connection error quotes the URL, which holds Telegram's token.
	if c := got["telegram token"]; c.OK || strings.Contains(c.Detail, tok) || !strings.Contains(c.Detail, "127.0.0.1") {
		t.Fatalf("telegram: %+v", c)
	}
	if c := got["model server"]; c.OK || !strings.Contains(c.Detail, "isn't reachable") {
		t.Fatalf("model server: %+v", c)
	}
	if c := got["server"]; c.OK || !c.Optional || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("server: %+v", c)
	}
}

func between(s, a, b string) string {
	_, s, _ = strings.Cut(s, a)
	s, _, _ = strings.Cut(s, b)
	return strings.TrimSpace(s)
}

// installWithMemories makes a data directory one schema version back with
// memories for Pip, as an older Thicket would have left it.
func installWithMemories(t *testing.T) *config.Config {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters")}
	known, _ := store.KnownMigrations()
	db, err := store.OpenAtSchema(ctx, filepath.Join(dir, "thicket.db"), known-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO users (id, channel, external_id, name, role, created_at) VALUES (1, 'discord', 'sam', 'Sam', 'owner', 1);
		INSERT INTO memories (user_id, character, kind, content, source, locked, created_at, updated_at) VALUES
		(1, 'pip', 'fact', 'Likes the dentist on Tuesdays.', 'auto', 0, 1, 1),
		(1, 'pip', 'preference', 'Wants reminders the day before.', 'user', 1, 1, 1),
		(1, '', 'person', 'Has a sister called Maya.', 'auto', 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.MkdirAll(cfg.CharactersDir, 0o755)
	os.WriteFile(filepath.Join(cfg.CharactersDir, "pip.md"), []byte("---\nid: pip\nname: Pip\nrole: Calendar.\npersonality:\n  traits: [tidy]\nvoice:\n  style: [brisk]\n---\nPip.\n"), 0o644)
	return cfg
}

func memoryCheck(t *testing.T, cfg *config.Config) Check {
	t.Helper()
	for _, c := range (&Doctor{Cfg: cfg, DataOnly: true}).Run(context.Background()) {
		if c.Name == "memory" {
			return c
		}
	}
	t.Fatal("no memory check")
	return Check{}
}

func TestMemoryCheckAfterMigration(t *testing.T) {
	cfg := installWithMemories(t)
	c := memoryCheck(t, cfg)
	if !c.OK || !strings.Contains(c.Detail, "3 memories") || !strings.Contains(c.Detail, "Pip 2") ||
		!strings.Contains(c.Detail, "all 3 memories from before the migration") {
		t.Fatalf("a clean migration: %+v", c)
	}
}

func TestMemoryCheckCatchesALossyMigration(t *testing.T) {
	cfg := installWithMemories(t)
	defer store.InjectMigration("DELETE FROM memories WHERE character = 'pip';")()
	c := memoryCheck(t, cfg)
	if c.OK || !strings.Contains(c.Detail, "lost in the migration") || !strings.Contains(c.Detail, "Pip's memories of Sam: 2 of 2 missing") {
		t.Fatalf("a migration that deleted Pip's memories: %+v", c)
	}
}

func TestMemoryCheckCatchesUnlockedMemories(t *testing.T) {
	cfg := installWithMemories(t)
	defer store.InjectMigration("UPDATE memories SET locked = 0;")()
	if c := memoryCheck(t, cfg); c.OK || !strings.Contains(c.Detail, "no longer locked") {
		t.Fatalf("a migration that unlocked memories: %+v", c)
	}
}

func TestMemoryCheckAfterUse(t *testing.T) {
	cfg := installWithMemories(t)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary use later: a person forgets Pip's memories, and a sprig
	// file is removed by hand. Neither fails the check.
	db.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	db.LogActivity(ctx, store.Activity{Level: "info", Kind: "memory", Summary: "forgot"})
	db.DB().ExecContext(ctx, "DELETE FROM memories WHERE character = 'pip' AND locked = 0")
	db.Close()
	os.Remove(filepath.Join(cfg.CharactersDir, "pip.md"))
	c := memoryCheck(t, cfg)
	if !c.OK || !strings.Contains(c.Detail, "has run since the migration") || !strings.Contains(c.Detail, `1 memories belong to sprig "pip"`) {
		t.Fatalf("after use: %+v", c)
	}
}
