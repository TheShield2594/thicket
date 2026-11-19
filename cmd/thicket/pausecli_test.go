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

func TestPauseCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters")}
	os.MkdirAll(cfg.CharactersDir, 0o755)
	os.WriteFile(filepath.Join(cfg.CharactersDir, "pip.md"), []byte("---\nid: pip\nname: Pip\nrole: Keeps the calendar.\npersonality:\n  traits: [upbeat]\nvoice:\n  style: [short]\n---\nPip.\n"), 0o644)
	run := func(on bool, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := pauseCmd(cfg, args, &out, on); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if out := run(true, "--reason", "something's off"); !strings.Contains(out, "Paused everything") {
		t.Fatal(out)
	}
	if out := run(true, "--sprig", "pip"); !strings.Contains(out, "Paused Pip") {
		t.Fatal(out)
	}
	if out := run(true, "status"); !strings.Contains(out, "everything") || !strings.Contains(out, "something's off") || !strings.Contains(out, "Pip") {
		t.Fatal(out)
	}
	if out := run(false, "--sprig", "pip"); !strings.Contains(out, "Everything is still paused") {
		t.Fatal(out)
	}
	run(false)
	if out := run(true, "status"); !strings.Contains(out, "Nothing is paused") {
		t.Fatal(out)
	}
	var b bytes.Buffer
	if err := pauseCmd(cfg, []string{"--sprig", "nobody"}, &b, true); err == nil {
		t.Fatal("paused a sprig that doesn't exist")
	}
	db, _ := openStore(cfg)
	defer db.Close()
	trail, _ := db.Trail(context.Background(), 0, 10)
	n := 0
	for _, e := range trail {
		if e.Kind == store.TrailPause {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("%d pause entries in the trail, want 4: %+v", n, trail)
	}
}
