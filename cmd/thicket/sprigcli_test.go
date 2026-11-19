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

func TestSprigPackageMemories(t *testing.T) {
	ctx := context.Background()
	newCfg := func() (*config.Config, *store.User) {
		d := t.TempDir()
		cfg := &config.Config{DataDir: d, CharactersDir: filepath.Join(d, "characters")}
		db, err := openStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		u, err := db.CreateUser(ctx, "web", "1", "Sam", "owner")
		if err != nil {
			t.Fatal(err)
		}
		return cfg, u
	}
	src, sam := newCfg()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.",
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}}
	md, _ := character.Marshal(c)
	os.MkdirAll(src.CharactersDir, 0o755)
	os.WriteFile(filepath.Join(src.CharactersDir, "fern.md"), md, 0o644)
	db, _ := openStore(src)
	kim, _ := db.CreateUser(ctx, "web", "2", "Kim", "member")
	db.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{
		{Action: "add", Memory: store.Memory{UserID: sam.ID, Character: "fern", Kind: "inside_joke", Content: "Calls the tomatoes the red team.", Source: "user", Locked: true}},
		{Action: "add", Memory: store.Memory{UserID: sam.ID, Kind: "person", Content: "Has a sister called Maya.", Source: "auto"}},
		{Action: "add", Memory: store.Memory{UserID: sam.ID, Kind: "fact", Content: "The household is vegetarian.", Source: "auto", Household: true}},
		{Action: "add", Memory: store.Memory{UserID: kim.ID, Character: "fern", Kind: "fact", Content: "Kim grows chillies on the balcony.", Source: "auto"}},
		{Action: "add", Memory: store.Memory{UserID: sam.ID, Character: "pip", Kind: "fact", Content: "Pip's own memory of the dentist.", Source: "auto"}},
	})
	db.Close()

	// Off by default.
	plain := filepath.Join(t.TempDir(), "plain.sprig")
	var out bytes.Buffer
	if err := exportSprigCmd(src, []string{"fern", "-o", plain}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No memories") {
		t.Fatalf("export without --memories: %s", out.String())
	}

	// --memories: the relationship only; --family adds family memory.
	pkg := filepath.Join(t.TempDir(), "fern.sprig")
	out.Reset()
	if err := exportSprigCmd(src, []string{"fern", "-o", pkg, "--memories", "--family"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 of Sam's memories") {
		t.Fatalf("export: %s", out.String())
	}

	dst, owner := newCfg()
	out.Reset()
	if err := importSprigCmd(dst, []string{pkg}, strings.NewReader("y\nl\n"), &out); err != nil {
		t.Fatal(err)
	}
	preview := out.String()
	for _, want := range []string{"[with Fern, inside_joke, set by them] Calls the tomatoes the red team.", "[family, fact] The household is vegetarian.", "Memories: 2 kept for Sam, locked."} {
		if !strings.Contains(preview, want) {
			t.Fatalf("missing %q:\n%s", want, preview)
		}
	}
	for _, never := range []string{"Maya", "chillies", "dentist"} {
		if strings.Contains(preview, never) {
			t.Fatalf("%q went into the package:\n%s", never, preview)
		}
	}
	db, _ = openStore(dst)
	defer db.Close()
	mine, _ := db.VisibleMemories(ctx, owner.ID, "fern")
	fam, _ := db.FamilyMemories(ctx, "")
	if len(mine) != 1 || mine[0].Character != "fern" || !mine[0].Locked || len(fam) != 1 || !fam[0].Locked {
		t.Fatalf("imported: mine %+v family %+v", mine, fam)
	}

	// Reviewable, under a new id: the memories go with the new id, and
	// what's already known isn't added twice.
	out.Reset()
	if err := importSprigCmd(dst, []string{pkg, "--yes", "--id", "fern2", "--name", "Fern Two", "--memories", "review"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Memories: 1 kept for Sam, reviewable.") || !strings.Contains(out.String(), "Not kept: already known: The household is vegetarian.") {
		t.Fatalf("second import:\n%s", out.String())
	}
	if mine, _ := db.VisibleMemories(ctx, owner.ID, "fern2"); len(mine) != 1 || mine[0].Character != "fern2" || mine[0].Locked {
		t.Fatalf("reviewable import: %+v", mine)
	}
}
