package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

// TestMain lets this test binary act as a local (stdio) file server, with
// THICKET_TEST_FILES_SERVER=1, so tests can start a real process.
func TestMain(m *testing.M) {
	if os.Getenv("THICKET_TEST_FILES_SERVER") == "1" {
		serveFiles()
		return
	}
	os.Exit(m.Run())
}

// serveFiles offers one read-only tool, read_file, that reads whatever
// path it's given, relative to where it runs: the server itself enforces
// nothing, so any refusal comes from Thicket.
func serveFiles() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					Path string `json:"path"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read_file", "annotations": map[string]any{"readOnlyHint": true},
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}}
		case "tools/call":
			b, err := os.ReadFile(req.Params.Arguments.Path)
			text := "CONTENT:" + string(b)
			if err != nil {
				text = "ERR:" + err.Error()
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		fmt.Fprintf(os.Stdout, "%s\n", out)
	}
}

const quill = `---
id: quill
name: Quill
role: Reads and files documents.
personality:
  traits: [careful]
voice:
  style: [short]
tools: ["mcp:files/*"]
---
Quill keeps papers.
`

// A sprig can't read another sprig's folder or the database through a
// local integration, whatever path it passes: relative, absolute, "..",
// or through a link planted in its own folder.
func TestSprigFilesStayInItsFolder(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := store.MCPServer{Name: "files", Transport: "stdio", Target: self, Enabled: true, Env: map[string]string{"THICKET_TEST_FILES_SERVER": "1"}}
	if err := a.DB.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MCP.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	chars := a.Cfg.CharactersDir
	os.WriteFile(filepath.Join(chars, "quill.md"), []byte(quill), 0o644)
	os.WriteFile(filepath.Join(chars, "pip.md"), []byte(pip), 0o644)
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	mine, theirs := filepath.Join(chars, "quill"), filepath.Join(chars, "pip")
	os.MkdirAll(mine, 0o755)
	os.MkdirAll(theirs, 0o755)
	os.WriteFile(filepath.Join(mine, "notes.txt"), []byte("quill's notes"), 0o644)
	os.WriteFile(filepath.Join(theirs, "diary.txt"), []byte("pip's diary"), 0o644)
	os.Symlink(theirs, filepath.Join(mine, "pip"))
	db := filepath.Join(a.Cfg.DataDir, "thicket.db")

	read := func(path string) string {
		t.Helper()
		a.Engine.HandleIncoming(ctx, dm("quill, read "+path))
		return sentText(fc.take())
	}
	if out := read("notes.txt"); !strings.Contains(out, "CONTENT:quill's notes") {
		t.Fatalf("its own file, relative to its folder:\n%s", out)
	}
	for _, path := range []string{"../pip/diary.txt", filepath.Join(theirs, "diary.txt"), "pip/diary.txt", "notes/../../pip/diary.txt", db, "../../thicket.db"} {
		out := read(path)
		if strings.Contains(out, "CONTENT:") || !strings.Contains(out, "isn't one of them") {
			t.Fatalf("%s was read:\n%s", path, out)
		}
	}

	// Its owner grants a folder outside: then it's readable, and only it.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "shared.txt"), []byte("shared doc"), 0o644)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	c, _ := a.Chars.Get("quill")
	if err := a.Engine.GrantPath(ctx, owner, c, a.Cfg.DataDir); err == nil {
		t.Fatal("Thicket's data folder was granted")
	}
	if err := a.Engine.GrantPath(ctx, owner, c, outside); err != nil {
		t.Fatal(err)
	}
	if out := read(filepath.Join(outside, "shared.txt")); !strings.Contains(out, "CONTENT:shared doc") {
		t.Fatalf("granted path:\n%s", out)
	}
	if out := read(filepath.Join(outside, "..", filepath.Base(a.Cfg.DataDir), "thicket.db")); strings.Contains(out, "CONTENT:") {
		t.Fatalf("escaped the grant:\n%s", out)
	}
	trail, _ := a.DB.Trail(ctx, 0, 50)
	found := false
	for _, e := range trail {
		found = found || e.Kind == store.TrailAccess && strings.Contains(e.Summary, outside)
	}
	if !found {
		t.Fatal("the grant isn't in the trail")
	}
}
