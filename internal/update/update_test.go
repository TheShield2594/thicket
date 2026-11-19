package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/doctor"
	"github.com/theshield2594/thicket/internal/store"
)

const notes = `# Changelog

## Unreleased: later

- not yet

## v0.3.0: the new one

- new things

## v0.2.0

- old things
`

// install makes a data directory as an older Thicket (one schema version
// back, v0.2.0) would have left it: a database with a person in it and a
// sprig folder.
func install(t *testing.T) *config.Config {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), HTTPAddr: "127.0.0.1:1"}
	known, err := store.KnownMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenAtSchema(ctx, filepath.Join(dir, "thicket.db"), known-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, "INSERT INTO settings (key, value, updated_at) VALUES (?, 'v0.2.0', 1), ('model', 'some/model', 1)", store.VersionKey); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.SchemaVersion(ctx); v != known-1 {
		t.Fatalf("old install is at schema v%d, want v%d", v, known-1)
	}
	db.Close()
	os.MkdirAll(filepath.Join(cfg.CharactersDir, "pip", "voice_tests"), 0o755)
	os.WriteFile(filepath.Join(cfg.CharactersDir, "pip.md"), []byte("---\nid: pip\n---\nPip.\n"), 0o644)
	os.WriteFile(filepath.Join(cfg.CharactersDir, "pip", "avatar.json"), []byte(`{"hue":120}`), 0o644)
	os.WriteFile(filepath.Join(cfg.CharactersDir, "pip", "voice_tests", "01.yaml"), []byte("prompt: hi\n"), 0o644)
	return cfg
}

// fingerprint hashes the database files and every sprig file: what an
// update may change and a rollback must put back.
func fingerprint(t *testing.T, cfg *config.Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(path string) {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(cfg.DataDir, path)
		out[rel] = hex.EncodeToString(sum[:])
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "thicket.db*"))
	for _, m := range matches {
		add(m)
	}
	filepath.WalkDir(cfg.CharactersDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			add(path)
		}
		return nil
	})
	return out
}

func healthy(context.Context) []doctor.Check {
	return []doctor.Check{{Name: "config", OK: true}, {Name: "database", OK: true, Detail: "integrity ok"}, {Name: "secrets", OK: true},
		// Offline, with no key: reported, but not a reason to undo an update.
		{Name: "model server", Detail: "not reachable"}, {Name: "server", Optional: true, Detail: "not running"}}
}

func updater(cfg *config.Config, out *bytes.Buffer) *Updater {
	return &Updater{Cfg: cfg, Version: "v0.3.0", Changelog: notes, Out: out, Doctor: healthy, Running: func() bool { return false }}
}

func TestUpdateFromOneVersionToTheNext(t *testing.T) {
	ctx := context.Background()
	cfg := install(t)
	known, _ := store.KnownMigrations()
	var out bytes.Buffer
	u := updater(cfg, &out)
	asked := ""
	u.Confirm = func(q string) bool { asked = q; return true }
	res, err := u.Run(ctx)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.Outcome != "updated" || res.From != "v0.2.0" || res.To != "v0.3.0" || res.FromSchema != known-1 || res.ToSchema != known {
		t.Fatalf("result: %+v", res)
	}
	text := out.String()
	// Only what's new since v0.2.0, up to this version.
	if asked == "" || !strings.Contains(text, "new things") || strings.Contains(text, "old things") || strings.Contains(text, "not yet") {
		t.Fatalf("changelog shown before asking:\n%s", text)
	}
	if !strings.Contains(text, "[warn] model server") {
		t.Fatalf("an unreachable model server should be a warning:\n%s", text)
	}

	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.SchemaVersion(ctx); v != known {
		t.Fatalf("schema v%d after the update, want v%d", v, known)
	}
	if v, _, _ := db.Setting(ctx, store.VersionKey); v != "v0.3.0" {
		t.Fatalf("recorded version %q", v)
	}
	if v, _, _ := db.Setting(ctx, "model"); v != "some/model" {
		t.Fatal("existing data didn't survive the update")
	}
	var summary string
	if err := db.DB().QueryRowContext(ctx, "SELECT summary FROM trail WHERE kind = ?", store.TrailUpdate).Scan(&summary); err != nil || !strings.Contains(summary, "v0.2.0 to v0.3.0") {
		t.Fatalf("trail entry: %q %v", summary, err)
	}
	// The backup taken first is a normal one, and holds the old schema.
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("pre-update backup: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.DataDir, "backups", ".pre-update-*")); len(left) != 0 {
		t.Fatalf("the working copy should be gone after a good update: %v", left)
	}
	hist, err := History(cfg.DataDir)
	if err != nil || len(hist) != 1 || hist[0].Outcome != "updated" {
		t.Fatalf("history: %+v %v", hist, err)
	}

	// Again: nothing to do, nothing asked.
	out.Reset()
	u.Confirm = func(string) bool { t.Fatal("asked about an update that isn't one"); return false }
	if res, err := u.Run(ctx); err != nil || res.Outcome != "nothing to do" {
		t.Fatalf("second run: %+v %v", res, err)
	}
}

func TestFailedCheckLeavesEverythingByteIdentical(t *testing.T) {
	ctx := context.Background()
	for name, breakIt := range map[string]func(*Updater, *config.Config){
		"doctor fails": func(u *Updater, _ *config.Config) {
			u.Doctor = func(context.Context) []doctor.Check {
				return []doctor.Check{{Name: "config", OK: true}, {Name: "database", Detail: "integrity check failed: injected"}}
			}
		},
		// A sprig file changed or added during the update is put back too.
		"doctor fails after sprig files changed": func(u *Updater, cfg *config.Config) {
			u.Doctor = func(context.Context) []doctor.Check {
				os.WriteFile(filepath.Join(cfg.CharactersDir, "pip.md"), []byte("scribbled over"), 0o644)
				os.WriteFile(filepath.Join(cfg.CharactersDir, "stray.md"), []byte("new"), 0o644)
				os.Remove(filepath.Join(cfg.CharactersDir, "pip", "avatar.json"))
				return []doctor.Check{{Name: "secrets", Detail: "can't be decrypted: injected"}}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := install(t)
			before := fingerprint(t, cfg)
			var out bytes.Buffer
			u := updater(cfg, &out)
			breakIt(u, cfg)
			res, err := u.Run(ctx)
			if err == nil || res.Outcome != "rolled back" || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("res %+v err %v\n%s", res, err, out.String())
			}
			after := fingerprint(t, cfg)
			if len(after) != len(before) {
				t.Fatalf("files before %v, after %v", before, after)
			}
			for f, sum := range before {
				if after[f] != sum {
					t.Errorf("%s changed across a rolled-back update", f)
				}
			}
			// The old version can go on using it: still its schema.
			known, _ := store.KnownMigrations()
			db, err := store.OpenExisting(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
			if err != nil {
				t.Fatal(err)
			}
			v, _ := db.SchemaVersion(ctx)
			ver, _, _ := db.Setting(ctx, store.VersionKey)
			db.Close()
			if v != known-1 || ver != "v0.2.0" {
				t.Fatalf("after rollback: schema v%d, version %q", v, ver)
			}
			// What survives a rollback: the log line and the backup.
			hist, _ := History(cfg.DataDir)
			if len(hist) != 1 || hist[0].Outcome != "rolled back" || !strings.Contains(hist[0].Detail, "injected") {
				t.Fatalf("history: %+v", hist)
			}
			if _, err := os.Stat(res.Backup); err != nil {
				t.Fatalf("the backup should be kept: %v", err)
			}
			if !strings.Contains(out.String(), "Everything is back exactly as it was") {
				t.Fatalf("output:\n%s", out.String())
			}
		})
	}
}

func TestUpdateRefusals(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer

	// No data yet.
	empty := &config.Config{DataDir: t.TempDir(), HTTPAddr: "127.0.0.1:1"}
	if _, err := updater(empty, &out).Run(ctx); err == nil || !strings.Contains(err.Error(), "thicket init") {
		t.Fatalf("no data: %v", err)
	}

	// Each refusal leaves the install exactly as it was.
	cfg := install(t)
	before := fingerprint(t, cfg)
	same := func(what string) {
		t.Helper()
		after := fingerprint(t, cfg)
		for f, sum := range before {
			if after[f] != sum {
				t.Errorf("%s: %s changed", what, f)
			}
		}
		if len(after) != len(before) {
			t.Errorf("%s: files before %d, after %d", what, len(before), len(after))
		}
	}

	u := updater(cfg, &out)
	u.Running = func() bool { return true }
	if _, err := u.Run(ctx); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running server: %v", err)
	}
	same("running server")

	u = updater(cfg, &out)
	u.Confirm = func(string) bool { return false }
	if _, err := u.Run(ctx); !errors.Is(err, ErrDeclined) {
		t.Fatalf("declined: %v", err)
	}
	same("declined")

	// An older version than the data last ran on, same schema: only with
	// --force. Across a schema change: never.
	if _, err := updater(cfg, &out).Run(ctx); err != nil {
		t.Fatal(err)
	}
	older := updater(cfg, &out)
	older.Version = "v0.2.5"
	if _, err := older.Run(ctx); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("downgrade without force: %v", err)
	}
	older.Force = true
	if res, err := older.Run(ctx); err != nil || res.Outcome != "updated" {
		t.Fatalf("forced downgrade on the same schema: %+v %v", res, err)
	}
	db, _ := store.OpenExisting(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
	db.DB().ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'from the future', 1)")
	db.Close()
	if _, err := older.Run(ctx); err == nil || !strings.Contains(err.Error(), "newer Thicket") {
		t.Fatalf("downgrade across a schema change: %v", err)
	}
}
