package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

// TestMain lets this test binary act as a stdio MCP server (with
// THICKET_TEST_STDIO_SERVER=1), so the tests can start a real process.
func TestMain(m *testing.M) {
	if os.Getenv("THICKET_TEST_STDIO_SERVER") == "1" {
		serveStdio()
		return
	}
	os.Exit(m.Run())
}

// serveStdio answers initialize, tools/list (one tool, "whoami") and
// tools/call with where it runs and what it was given.
func serveStdio() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "whoami", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			wd, _ := os.Getwd()
			_, leaked := os.LookupEnv("THICKET_SECRET_KEY")
			b, _ := json.Marshal(map[string]any{"dir": wd, "home": os.Getenv("HOME"), "leaked": leaked, "given": os.Getenv("GIVEN")})
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		fmt.Fprintf(os.Stdout, "%s\n", out)
	}
}

type passSecrets struct{}

func (passSecrets) Expand(_ context.Context, v string) (string, error) { return v, nil }

func TestStdioServersRunPerSprigWithScrubbedEnv(t *testing.T) {
	t.Setenv("THICKET_SECRET_KEY", "must-not-leak")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := store.MCPServer{Name: "local", Transport: "stdio", Target: self, Enabled: true,
		Env: map[string]string{"THICKET_TEST_STDIO_SERVER": "1", "GIVEN": "configured"}}
	if err := db.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(db, passSecrets{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.Close()
	chars := filepath.Join(dir, "characters")
	r.Home = filepath.Join(dir, "mcp-home")
	r.Folder = func(sprig string) (string, error) {
		d := filepath.Join(dir, "mcp")
		if sprig != "" {
			d = filepath.Join(chars, sprig)
		}
		return d, os.MkdirAll(d, 0o700)
	}
	if _, err := r.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}

	type where struct {
		Dir, Home, Given string
		Leaked           bool
	}
	ask := func(sprig string) where {
		t.Helper()
		res, err := r.CallAs(ctx, sprig, "local", "whoami", json.RawMessage(`{}`), true, false)
		if err != nil {
			t.Fatal(err)
		}
		var w where
		if err := json.Unmarshal([]byte(res.Text), &w); err != nil {
			t.Fatalf("%s: %v", res.Text, err)
		}
		return w
	}
	resolved := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	for _, sprig := range []string{"pip", "bramble"} {
		w := ask(sprig)
		if resolved(w.Dir) != resolved(filepath.Join(chars, sprig)) {
			t.Errorf("%s's process runs in %s", sprig, w.Dir)
		}
		if w.Leaked {
			t.Errorf("%s's process can see THICKET_SECRET_KEY", sprig)
		}
		if w.Given != "configured" || w.Home != r.Home {
			t.Errorf("%s's process env: given %q, home %q", sprig, w.Given, w.Home)
		}
	}
	if len(r.Tools()) != 1 {
		t.Fatalf("per-sprig processes are listed as tools: %v", r.Tools())
	}
	// Re-marked exec while connected: the connection that serves the call
	// decides, so a sprig without exec is refused, and one with it isn't.
	s.Exec = true
	if err := db.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallAs(ctx, "pip", "local", "whoami", json.RawMessage(`{}`), true, false); !errors.Is(err, ErrExec) {
		t.Fatalf("exec integration without exec access: %v", err)
	}
	if _, err := r.CallReadOnlyAs(ctx, "pip", "local", "whoami", json.RawMessage(`{}`), false); !errors.Is(err, ErrExec) {
		t.Fatalf("read-only call to an exec integration without exec access: %v", err)
	}
	if _, err := r.CallAs(ctx, "pip", "local", "whoami", json.RawMessage(`{}`), true, true); err != nil {
		t.Fatalf("exec integration with exec access: %v", err)
	}
	r.Disconnect("local")
	r.mu.Lock()
	n := len(r.conns)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d connections left after removing the server", n)
	}
}
