package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// A member may give plugins, and only plugins, to their own private
// sprig; not to anyone else's, and not to a shared one.
func TestMemberGrantsPluginsToOwnSprig(t *testing.T) {
	ctx := context.Background()
	f := newTaintFixture(t)
	kim, err := f.e.DB.CreateUser(ctx, "web", "kim", "Kim", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.e.Cfg.DataDir, "characters")
	os.MkdirAll(dir, 0o755)
	write := func(id, extra string) {
		src := "---\nid: " + id + "\nname: " + strings.ToUpper(id[:1]) + id[1:] + "\nrole: Helps.\n" + extra +
			"personality:\n  traits: [quiet]\nvoice:\n  style: [short]\n---\nA sprig.\n"
		if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("wren", fmt.Sprintf("belongs_to: %d\n", kim.ID))
	write("ledger", fmt.Sprintf("belongs_to: %d\n", f.user.ID))
	write("hal", "")
	f.e.Chars = character.NewRegistry(dir, f.e.DB, f.e.Log)
	if err := f.e.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	// Written again through the registry, so it knows they weren't edited by hand.
	for _, id := range []string{"wren", "ledger", "hal"} {
		c, _ := f.e.Chars.Get(id)
		os.Remove(filepath.Join(dir, id+".md"))
		if _, err := f.e.Chars.Write(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(sprig, scope string) error {
		_, err := f.e.toolGrantTools(ctx, &turn{user: kim, char: f.char, chatRef: "web:kim"},
			json.RawMessage(`{"character":"`+sprig+`","add":["`+scope+`"]}`))
		return err
	}
	if err := grant("wren", "mcp:sandbox/*"); err != nil {
		t.Fatalf("Kim couldn't give her own sprig a plugin: %v", err)
	}
	if c, _ := f.e.Chars.Get("wren"); !strings.Contains(strings.Join(c.Tools, ","), "mcp:sandbox/*") {
		t.Fatalf("not granted: %v", c.Tools)
	}
	for _, c := range []struct{ sprig, scope string }{
		{"wren", "mcp:ha/*"}, // not a plugin
		{"wren", "web"},
		{"hal", "mcp:sandbox/*"},    // shared
		{"ledger", "mcp:sandbox/*"}, // someone else's
	} {
		if err := grant(c.sprig, c.scope); err == nil {
			t.Errorf("Kim granted %s to %s", c.scope, c.sprig)
		}
	}
	// The owner (an admin) still grants anything to a shared sprig.
	if _, err := f.e.toolGrantTools(ctx, &turn{user: f.user, char: f.char, chatRef: "web:1"},
		json.RawMessage(`{"character":"hal","add":["mcp:ha/*"]}`)); err != nil {
		t.Fatal(err)
	}
}
