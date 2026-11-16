package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

func TestExportRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	chars := filepath.Join(src, "characters")
	os.MkdirAll(chars, 0o755)
	os.WriteFile(filepath.Join(chars, "pip.md"), []byte("---\nid: pip\n---\n"), 0o644)
	os.MkdirAll(filepath.Join(chars, "pip", "voice_tests"), 0o755)
	os.WriteFile(filepath.Join(chars, "pip", "voice_tests", "01-hello.yaml"), []byte("prompt: hi\nreference: Hello!\n"), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "voice_tests", "02.dotted.name.yaml"), []byte("prompt: yo\n"), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "voice_tests", "03 with space.yaml"), []byte("prompt: hey\n"), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "voice_tests", ".hidden.yaml"), []byte("prompt: psst\n"), 0o644)
	// Names that once mapped to the same staging file.
	os.MkdirAll(filepath.Join(chars, "pip__voice_tests__x", "voice_tests"), 0o755)
	os.WriteFile(filepath.Join(chars, "pip__voice_tests__x", "voice_tests", "01.yaml"), []byte("prompt: a\n"), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "voice_tests", "x__voice_tests__01.yaml"), []byte("prompt: b\n"), 0o644)
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
	if b, _ := os.ReadFile(filepath.Join(dchars, "pip", "voice_tests", "01-hello.yaml")); !strings.Contains(string(b), "reference: Hello!") {
		t.Fatal("voice tests not restored")
	}
	// Every restored voice test is where the loader looks; nothing nested
	// was flattened into the characters folder.
	if tests, err := character.LoadVoiceTests(dchars, "pip"); err != nil || len(tests) != 5 {
		t.Fatalf("voice tests after restore: %d %v", len(tests), err)
	}
	if _, err := os.Stat(filepath.Join(dchars, ".hidden.yaml")); err == nil {
		t.Fatal("a nested file was flattened")
	}
	for _, f := range []string{"pip/voice_tests/02.dotted.name.yaml", "pip/voice_tests/03 with space.yaml", "pip/voice_tests/.hidden.yaml", "pip__voice_tests__x/voice_tests/01.yaml", "pip/voice_tests/x__voice_tests__01.yaml"} {
		if _, err := os.Stat(filepath.Join(dchars, f)); err != nil {
			t.Fatalf("%s not restored: %v", f, err)
		}
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

func TestRestoreLeavesInstallUntouchedOnBadArchive(t *testing.T) {
	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "thicket.db"), []byte("old db"), 0o600)
	os.WriteFile(filepath.Join(dst, "secret.key"), []byte("old key"), 0o600)
	// An archive with a key but no database must not swap the key in.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "secret.key", Mode: 0o600, Size: 7, Typeflag: tar.TypeReg})
	tw.Write([]byte("new key"))
	tw.Close()
	gz.Close()
	if err := Restore(&buf, dst, filepath.Join(dst, "characters"), true); err == nil {
		t.Fatal("expected failure")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "secret.key")); string(b) != "old key" {
		t.Fatalf("key replaced by a failed restore: %q", b)
	}
}
