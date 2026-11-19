package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/sandbox"
)

const tinker = `---
id: tinker
name: Tinker
role: Builds things on the sandbox computer.
personality:
  traits: [handy]
voice:
  style: [short]
tools: ["mcp:sandbox/*"]
---
Tinker likes tools.
`

func TestSandboxComputer(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	os.MkdirAll(ws, 0o755)
	box := &sandbox.Server{Workdir: ws, TokenFile: filepath.Join(dir, "auth", "token")}
	srv := httptest.NewServer(box.Handler())
	defer srv.Close()

	a.Cfg.SandboxURL = srv.URL
	a.Cfg.SandboxTokenFile = box.TokenFile
	a.ensureSandbox(ctx)
	// The token is shared through the file, and the integration stored once.
	tok, err := os.ReadFile(box.TokenFile)
	if err != nil || len(strings.TrimSpace(string(tok))) < 32 {
		t.Fatalf("token file: %q %v", tok, err)
	}
	a.ensureSandbox(ctx) // idempotent: same token
	if again, _ := os.ReadFile(box.TokenFile); string(again) != string(tok) {
		t.Fatal("token changed on restart")
	}
	servers, _ := a.DB.MCPServers(ctx)
	if len(servers) != 1 || servers[0].Name != "sandbox" || strings.Contains(servers[0].Headers["Authorization"], strings.TrimSpace(string(tok))) {
		t.Fatalf("stored integration: %+v", servers)
	}
	if _, err := a.MCP.Connect(ctx, servers[0]); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "tinker.md"), []byte(tinker), 0o644)
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	// Running a command asks first, and runs once approved.
	a.Engine.HandleIncoming(ctx, dm("tinker, build it on your computer"))
	if out := sentText(fc.take()); !strings.Contains(out, "echo built") || !strings.Contains(out, "approve") {
		t.Fatalf("no approval prompt:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err == nil {
		t.Fatal("command ran before approval")
	}
	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if b, err := os.ReadFile(filepath.Join(ws, "out.txt")); err != nil || string(b) != "built\n" {
		t.Fatalf("command didn't run after approval: %q %v", b, err)
	}
	assertNoLeak(t, a, strings.TrimSpace(string(tok)))
}
