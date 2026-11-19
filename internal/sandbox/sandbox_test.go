package sandbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	os.MkdirAll(ws, 0o755)
	s := &Server{Workdir: ws, TokenFile: filepath.Join(dir, "token")}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func call(t *testing.T, url, token string, id int, method string, params any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequest("POST", url+"/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func toolText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	r, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
}

func TestSandbox(t *testing.T) {
	s, srv := setup(t)
	// No token yet: not ready.
	if code, _ := call(t, srv.URL, "", 1, "initialize", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("before token: %d", code)
	}
	os.WriteFile(s.TokenFile, []byte("tok-123\n"), 0o644)
	if code, _ := call(t, srv.URL, "wrong", 1, "initialize", nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	// A new token from Thicket is picked up without a restart.
	os.WriteFile(s.TokenFile, []byte("tok-456\n"), 0o644)
	if code, _ := call(t, srv.URL, "tok-456", 1, "initialize", nil); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	if code, _ := call(t, srv.URL, "tok-123", 1, "initialize", nil); code != http.StatusUnauthorized {
		t.Fatalf("old token still works: %d", code)
	}
	os.WriteFile(s.TokenFile, []byte("tok-123\n"), 0o644)
	if _, out := call(t, srv.URL, "tok-123", 1, "tools/list", nil); len(out["result"].(map[string]any)["tools"].([]any)) != 4 {
		t.Fatalf("tools: %v", out)
	}

	run := func(args map[string]any) (string, bool) {
		_, out := call(t, srv.URL, "tok-123", 2, "tools/call", map[string]any{"name": "run_command", "arguments": args})
		return toolText(t, out)
	}
	if text, isErr := run(map[string]any{"command": "echo hello; echo oops >&2; pwd; exit 3"}); isErr ||
		!strings.Contains(text, "Exit code 3.") || !strings.Contains(text, "stdout:\nhello\n"+s.Workdir) || !strings.Contains(text, "stderr:\noops") {
		t.Fatalf("run: %q", text)
	}
	start := time.Now()
	if text, _ := run(map[string]any{"command": "sleep 30", "timeout_seconds": 1}); !strings.Contains(text, "Stopped after 1s") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %q after %s", text, time.Since(start))
	}
	// A background process doesn't outlive its command.
	run(map[string]any{"command": "(sleep 3; touch late) & echo started"})
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(s.Workdir, "late")); err == nil {
		t.Fatal("background process kept running")
	}
	if text, _ := run(map[string]any{"command": "yes | head -c 100000"}); !strings.Contains(text, "[earlier output cut]") || len(text) > maxOutput+200 {
		t.Fatalf("output not capped: %d bytes", len(text))
	}

	tool := func(name string, args map[string]any) (string, bool) {
		_, out := call(t, srv.URL, "tok-123", 3, "tools/call", map[string]any{"name": name, "arguments": args})
		return toolText(t, out)
	}
	if text, isErr := tool("write_file", map[string]any{"path": "notes/a.txt", "content": "café"}); isErr || !strings.Contains(text, "Wrote 5 bytes") {
		t.Fatalf("write: %q", text)
	}
	if text, isErr := tool("read_file", map[string]any{"path": "notes/a.txt"}); isErr || text != "café" {
		t.Fatalf("read: %q", text)
	}
	if text, _ := tool("list_files", map[string]any{}); !strings.Contains(text, "notes/") {
		t.Fatalf("list: %q", text)
	}
	os.WriteFile(filepath.Join(s.Workdir, "bin.dat"), []byte{0xff, 0xfe, 0x00, 0x80, 0x81, 0x82, 0x83}, 0o644)
	if text, isErr := tool("read_file", map[string]any{"path": "bin.dat"}); !isErr || !strings.Contains(text, "isn't text") {
		t.Fatalf("binary: %q", text)
	}
	if text, isErr := tool("read_file", map[string]any{"path": "missing.txt"}); !isErr || !strings.Contains(text, "no such file") {
		t.Fatalf("missing: %q", text)
	}
	if _, out := call(t, srv.URL, "tok-123", 4, "tools/call", map[string]any{"name": "format_disk"}); out["error"] == nil {
		t.Fatalf("unknown tool: %v", out)
	}
}
