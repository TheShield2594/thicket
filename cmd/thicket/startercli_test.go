package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

func TestSprigStarterCLI(t *testing.T) {
	d := t.TempDir()
	cfg := &config.Config{DataDir: d, CharactersDir: filepath.Join(d, "characters")}
	db, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.CreateUser(ctx, "web", "1", "Sam", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "web", "2", "Mia", store.RoleKid); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out bytes.Buffer
	if err := sprigCmd(cfg, []string{"starter"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"homework-helper", "reading-buddy", "meal-planner", "bill-tracker", "chore-chart"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("list has no %s:\n%s", id, out.String())
		}
	}

	// Declined: nothing written.
	out.Reset()
	if err := sprigCmd(cfg, []string{"starter", "meal-planner"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "+++ b/characters/basil.md") || !strings.Contains(out.String(), "Nothing was written") {
		t.Fatalf("preview:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.CharactersDir, "basil.md")); err == nil {
		t.Fatal("declined starter was written")
	}

	// A kid starter needs the kid.
	if err := sprigCmd(cfg, []string{"starter", "reading-buddy", "--yes"}, nil, &out); err == nil || !strings.Contains(err.Error(), "--kid") {
		t.Fatalf("kid without --kid: %v", err)
	}
	out.Reset()
	if err := sprigCmd(cfg, []string{"starter", "meal-planner", "reading-buddy", "--kid", "Mia", "--yes"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"basil.md", "poppy.md", "poppy/avatar.json", "basil/voice_tests/01-hello.yaml"} {
		if _, err := os.Stat(filepath.Join(cfg.CharactersDir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	db, _ = openStore(cfg)
	defer db.Close()
	jobs, _ := db.CharacterJobs(ctx, "basil")
	if len(jobs) != 1 || jobs[0].Enabled {
		t.Fatalf("routines should arrive paused: %+v", jobs)
	}

	// Suggested check-ins are listed, stay off by default (Basil above)
	// and go on with --check-ins.
	if b, _ := os.ReadFile(filepath.Join(cfg.CharactersDir, "basil.md")); strings.Contains(string(b), "due_reminders") {
		t.Fatalf("basil's check-ins are on without --check-ins:\n%s", b)
	}
	out.Reset()
	if err := sprigCmd(cfg, []string{"starter", "chore-chart", "--check-ins", "--yes"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.CharactersDir, "thyme.md")); !strings.Contains(string(b), "morning_brief: \"07:30\"") && !strings.Contains(string(b), "morning_brief: 07:30") {
		t.Fatalf("thyme's check-ins aren't on:\n%s", b)
	}

	// Each starter is also a package any Thicket can import.
	dir := filepath.Join(t.TempDir(), "pkgs")
	out.Reset()
	if err := sprigCmd(cfg, []string{"starter", "--export", dir}, nil, &out); err != nil {
		t.Fatal(err)
	}
	p, err := sprigpkg.ReadFile(filepath.Join(dir, "chore-chart.sprig"))
	if err != nil || p.Char.ID != "thyme" || len(p.Skills) != 1 || len(p.Routines) != 1 {
		t.Fatalf("exported package: %v %+v", err, p)
	}
}
