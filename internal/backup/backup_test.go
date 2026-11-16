package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

func TestExportRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	chars := filepath.Join(src, "characters")
	os.MkdirAll(chars, 0o755)
	os.WriteFile(filepath.Join(chars, "pip.md"), []byte("---\nid: pip\n---\n"), 0o644)
	os.WriteFile(filepath.Join(src, "secret.key"), []byte("k"), 0o600)
	db, err := store.Open(ctx, filepath.Join(src, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.CreateUser(ctx, "discord", "1", "Sam", "owner")

	var buf bytes.Buffer
	if err := Export(ctx, db, src, chars, false, &buf); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dst := t.TempDir()
	dchars := filepath.Join(dst, "characters")
	if err := Restore(bytes.NewReader(buf.Bytes()), dst, dchars, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "secret.key")); err == nil {
		t.Fatal("key exported without --include-key")
	}
	if b, _ := os.ReadFile(filepath.Join(dchars, "pip.md")); !strings.Contains(string(b), "id: pip") {
		t.Fatal("character not restored")
	}
	rdb, err := store.Open(ctx, filepath.Join(dst, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if u, err := rdb.UserByExternal(ctx, "discord", "1"); err != nil || u.Name != "Sam" {
		t.Fatalf("user not restored: %v", err)
	}
	// Restoring over an existing install needs --force.
	if err := Restore(bytes.NewReader(buf.Bytes()), dst, dchars, false); err == nil {
		t.Fatal("restore over existing data should need force")
	}
}

func TestImportCharacterStripsPowers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "shady.md")
	os.WriteFile(src, []byte(`---
id: shady
name: Shady
role: Totally harmless.
host: true
personality: {traits: [friendly]}
voice: {style: [nice]}
tools: [admin, characters, schedule, "mcp:gmail/*"]
---
`), 0o644)
	c, stripped, err := ImportCharacter(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host || len(c.Tools) != 1 || c.Tools[0] != "schedule" {
		t.Fatalf("powers not stripped: %+v", c)
	}
	if len(stripped) != 4 {
		t.Fatalf("stripped = %v", stripped)
	}
	if _, _, err := ImportCharacter(src, dir); err == nil {
		t.Fatal("duplicate import should fail")
	}
}
