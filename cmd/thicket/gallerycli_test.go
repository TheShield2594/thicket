package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

func TestGalleryCLI(t *testing.T) {
	ctx := context.Background()
	newCfg := func() *config.Config {
		d := t.TempDir()
		cfg := &config.Config{DataDir: d, CharactersDir: filepath.Join(d, "characters")}
		db, err := openStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateUser(ctx, "web", "1", "Sam", "owner"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		return cfg
	}
	src, dst := newCfg(), newCfg()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.", BelongsTo: 1, Household: character.TierSolo,
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}, Tools: []string{"schedule", "mcp:gmail/*"}}
	md, _ := character.Marshal(c)
	os.MkdirAll(character.SkillDir(src.CharactersDir, "fern"), 0o755)
	os.WriteFile(filepath.Join(src.CharactersDir, "fern.md"), md, 0o644)
	db, _ := openStore(src)
	if _, err := db.SaveSkill(ctx, store.Skill{ID: "seed-swap", Name: "Seed swap", Description: "Use when trading seeds", Body: "1. list spares", SprigID: "fern"}, 0); err != nil {
		t.Fatal(err)
	}
	db.Close()

	pkg := filepath.Join(t.TempDir(), "fern.sprig")
	var out bytes.Buffer
	if err := exportSprigCmd(src, []string{"fern", "--for-gallery", "-o", pkg}, &out); err == nil || !strings.Contains(err.Error(), "gallery keygen") {
		t.Fatalf("export without a key: %v", err)
	}
	if err := galleryCmd(src, []string{"keygen"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := galleryCmd(src, []string{"keygen"}, &out); err == nil {
		t.Fatal("a second keygen replaced the key")
	}
	out.Reset()
	if err := galleryCmd(src, []string{"key"}, &out); err != nil {
		t.Fatal(err)
	}
	keyID := regexp.MustCompile(`Author key ([0-9a-f]{16})`).FindStringSubmatch(out.String())[1]
	if err := exportSprigCmd(src, []string{"fern", "--for-gallery", "--memories", "-o", pkg}, &out); err == nil {
		t.Fatal("--memories went into a gallery export")
	}
	if err := exportSprigCmd(src, []string{"fern", "--for-gallery", "-o", pkg}, &out); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := importSprigCmd(dst, []string{pkg}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Signed by author key " + keyID, "New author", "fern/skills/seed-swap.md.pending", "schedule, mcp:gmail/*", "Nothing was written"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("preview lacks %q:\n%s", s, out.String())
		}
	}
	out.Reset()
	if err := importSprigCmd(dst, []string{pkg, "--yes"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "waiting for your approval: seed-swap") {
		t.Fatalf("output:\n%s", out.String())
	}
	b, _ := os.ReadFile(filepath.Join(dst.CharactersDir, "fern.md"))
	got, err := character.Parse(b)
	if err != nil || len(got.Tools) != 0 || got.BelongsTo != 0 {
		t.Fatalf("imported %+v (%v)", got, err)
	}
	ddb, _ := openStore(dst)
	if lv, _ := ddb.ToolLevel(ctx, "fern", "schedule_job", store.LevelFor{Granted: true}); lv != store.LevelDraft {
		t.Fatalf("level %q", lv)
	}
	ddb.Close()

	// Importing it again is an update from the same author.
	out.Reset()
	if err := importSprigCmd(dst, []string{pkg, "--yes"}, nil, &out); err != nil || !strings.Contains(out.String(), "Updated Fern") {
		t.Fatalf("update: %v\n%s", err, out.String())
	}
	// A revoked author is refused.
	if err := galleryCmd(dst, []string{"revoke", keyID}, &out); err != nil {
		t.Fatal(err)
	}
	if err := importSprigCmd(dst, []string{pkg, "--yes"}, nil, &out); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked: %v", err)
	}
}
