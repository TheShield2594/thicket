package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

func TestSprigPackageCLI(t *testing.T) {
	newCfg := func() *config.Config {
		d := t.TempDir()
		cfg := &config.Config{DataDir: d, CharactersDir: filepath.Join(d, "characters")}
		db, err := openStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateUser(context.Background(), "web", "1", "Sam", "owner"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		return cfg
	}
	src, dst := newCfg(), newCfg()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.",
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}, Tools: []string{"schedule", "mcp:gmail/*"}}
	md, _ := character.Marshal(c)
	os.MkdirAll(src.CharactersDir, 0o755)
	os.WriteFile(filepath.Join(src.CharactersDir, "fern.md"), md, 0o644)

	pkg := filepath.Join(t.TempDir(), "fern.sprig")
	var out bytes.Buffer
	if err := exportSprigCmd(src, []string{"Fern", "-o", pkg}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := importSprigCmd(dst, []string{pkg}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "+++ b/characters/fern.md") || !strings.Contains(out.String(), "mcp:gmail/*") {
		t.Fatalf("preview:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dst.CharactersDir, "fern.md")); err == nil {
		t.Fatal("declined import was written")
	}
	if err := importSprigCmd(dst, []string{pkg, "--yes"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	db, _ := openStore(dst)
	defer db.Close()
	if lv, _ := db.ToolLevel(context.Background(), "fern", "schedule_job", true); lv != store.LevelDraft {
		t.Fatalf("level %q", lv)
	}
	err := importSprigCmd(dst, []string{pkg, "--yes"}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), `--id "fern-2"`) {
		t.Fatalf("clash: %v", err)
	}
}
