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
	"time"

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

// archive builds a backup by hand, for the refusals below.
func archive(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestRestoreChecksManifestFirst(t *testing.T) {
	db := append([]byte("SQLite format 3\x00"), make([]byte, 100)...)
	cases := map[string]map[string][]byte{
		"no manifest":      {"thicket.db": db},
		"newer format":     {"thicket.db": db, "manifest.json": []byte(`{"format": 99, "schema_version": 1}`)},
		"newer schema":     {"thicket.db": db, "manifest.json": []byte(`{"format": 2, "schema_version": 9999}`)},
		"broken manifest":  {"thicket.db": db, "manifest.json": []byte(`{`)},
		"not a database":   {"thicket.db": []byte("hello, this is not sqlite"), "manifest.json": []byte(`{"format": 2, "schema_version": 1}`)},
		"unsafe file path": {"thicket.db": db, "manifest.json": []byte(`{"format": 2, "schema_version": 1}`), "characters/../../evil": []byte("x")},
	}
	for name, files := range cases {
		dst := t.TempDir()
		if err := Restore(archive(t, files), dst, filepath.Join(dst, "characters"), false); err == nil {
			t.Errorf("%s: restore should be refused", name)
		}
		left, _ := os.ReadDir(dst)
		if len(left) != 0 {
			t.Errorf("%s: a refused restore left %d entries behind", name, len(left))
		}
	}
}

func TestRestoreRefusedWhileServerRuns(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(src, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Export(ctx, db, src, filepath.Join(src, "characters"), false, &buf); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dst := t.TempDir()
	if ServerRunning(dst) {
		t.Fatal("nothing has run here yet")
	}
	if err := Heartbeat(dst); err != nil {
		t.Fatal(err)
	}
	err = Restore(bytes.NewReader(buf.Bytes()), dst, filepath.Join(dst, "characters"), false)
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("restore under a running server: %v", err)
	}
	// A server that stopped, or one whose last beat is old, is no obstacle.
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(filepath.Join(dst, aliveFile), old, old)
	if ServerRunning(dst) {
		t.Fatal("a stale heartbeat still counts as running")
	}
	if err := Restore(bytes.NewReader(buf.Bytes()), dst, filepath.Join(dst, "characters"), false); err != nil {
		t.Fatal(err)
	}
	ClearHeartbeat(dst)
}

func TestScheduledBackupsRestoreAndPrune(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	chars := filepath.Join(src, "characters")
	os.MkdirAll(filepath.Join(chars, "pip", "notes", "deep"), 0o755)
	os.WriteFile(filepath.Join(chars, "pip.md"), []byte("---\nid: pip\n---\n"), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "avatar.json"), []byte(`{"hue": 120}`), 0o644)
	os.WriteFile(filepath.Join(chars, "pip", "notes", "deep", "a.txt"), []byte("kept"), 0o644)
	os.WriteFile(filepath.Join(src, "secret.key"), []byte("k"), 0o600)
	db, err := store.Open(ctx, filepath.Join(src, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.CreateUser(ctx, "discord", "1", "Sam", "owner")

	dir := filepath.Join(t.TempDir(), "out")
	// Older backups, and a stranger's file that must be left alone.
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"thicket-backup-20200101-000000.tar.gz", "thicket-backup-20200102-000000.tar.gz", "thicket-backup-20200103-000000.tar.gz", "holiday.tar.gz"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	info, err := Scheduled(ctx, db, src, chars, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if latest, ok := Latest(dir); !ok || latest.Path != info.Path || latest.Size != info.Size || info.Size < 100 {
		t.Fatalf("Latest = %+v, wrote %+v", latest, info)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 3 || names[0] != "holiday.tar.gz" || names[1] != "thicket-backup-20200103-000000.tar.gz" {
		t.Fatalf("after pruning to 2: %v", names)
	}

	// The scheduled archive restores into an empty data directory, with
	// the sprig's whole folder and without the key.
	f, err := os.Open(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dst := t.TempDir()
	dchars := filepath.Join(dst, "characters")
	if err := Restore(f, dst, dchars, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pip.md", "pip/avatar.json", "pip/notes/deep/a.txt"} {
		if _, err := os.Stat(filepath.Join(dchars, name)); err != nil {
			t.Errorf("%s not restored: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "secret.key")); err == nil {
		t.Error("a scheduled backup must never carry the key")
	}
	rdb, err := store.Open(ctx, filepath.Join(dst, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if u, err := rdb.UserByExternal(ctx, "discord", "1"); err != nil || u.Name != "Sam" {
		t.Fatalf("user not restored: %v", err)
	}
}
