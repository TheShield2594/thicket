package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

func TestCharacterImportPreviewThenWrite(t *testing.T) {
	data := t.TempDir()
	cfg := &config.Config{DataDir: data, CharactersDir: filepath.Join(data, "characters")}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "IDENTITY.md"), []byte("- **Name:** Clawdia\n- **Vibe:** sharp, warm\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("Be genuinely helpful."), 0o644)
	os.WriteFile(filepath.Join(ws, "USER.md"), []byte("- **Name:** Sam\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte("- Sam runs on Sundays.\n"), 0o644)

	db, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateUser(context.Background(), "discord", "1", "Sam", "owner")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Saying no writes nothing.
	var out bytes.Buffer
	if err := characterImportCmd(cfg, []string{ws, "--memories"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--- characters/clawdia.md") || !strings.Contains(out.String(), "[just Clawdia] Sam runs on Sundays.") {
		t.Fatalf("preview:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.CharactersDir, "clawdia.md")); err == nil {
		t.Fatal("declined import was written")
	}

	out.Reset()
	if err := characterImportCmd(cfg, []string{"--memories", ws}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CharactersDir, "clawdia.md")); err != nil {
		t.Fatalf("not written: %v\n%s", err, out.String())
	}
	db, err = openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mems, err := db.AllMemories(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("memories = %+v", mems)
	}
	for _, m := range mems {
		if !m.Locked || m.Source != "user" {
			t.Fatalf("memory not saved as the user's: %+v", m)
		}
		if strings.Contains(m.Content, "Sundays") != (m.Character == "clawdia") {
			t.Fatalf("wrong scope: %+v", m)
		}
	}
	if lvl, _ := db.ToolLevel(context.Background(), "clawdia", "anything", true); lvl != store.LevelDraft {
		t.Fatalf("level = %q", lvl)
	}

	// A second import of the same sprig is refused before any preview.
	if err := characterImportCmd(cfg, []string{ws, "--yes"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("duplicate import should fail")
	}
}
