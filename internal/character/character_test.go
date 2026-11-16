package character

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memHashes map[string]string

func (m memHashes) CharacterFileHash(_ context.Context, id string) (string, error) { return m[id], nil }
func (m memHashes) SetCharacterFileHash(_ context.Context, id, h string) error {
	m[id] = h
	return nil
}

func newReg(t *testing.T) (*Registry, string) {
	dir := t.TempDir()
	return NewRegistry(dir, memHashes{}, slog.New(slog.NewTextHandler(io.Discard, nil))), dir
}

func TestDefaultJuniperIsValid(t *testing.T) {
	b, err := defaultsFS.ReadFile("defaults/juniper.md")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if probs := c.Validate(); len(probs) > 0 {
		t.Fatalf("juniper invalid: %v", probs)
	}
	if !c.Host || c.DisplayName() != "Juni" || !c.HasScope(ScopeCharacters) {
		t.Fatalf("unexpected juniper: %+v", c)
	}
	if !strings.Contains(c.Body, "watering can") {
		t.Fatal("body not parsed")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "name: x\n",
		"unclosed":       "---\nid: x\n",
		"unknown field":  "---\nid: pip\nname: Pip\nfavourite_colour: red\n---\n",
		"bad yaml":       "---\nid: [\n---\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidate(t *testing.T) {
	c := &Character{ID: "Pip!", Tools: []string{"rm -rf", "mcp:notion/*", "mcp:ha/turn_on"}, Color: "red",
		Initiative: Initiative{QuietHours: "late"}}
	probs := strings.Join(c.Validate(), "\n")
	for _, want := range []string{"id", "name is required", "role is required", "traits", "voice.style", "color", `"rm -rf"`, "quiet_hours"} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing problem about %q in:\n%s", want, probs)
		}
	}
	if strings.Contains(probs, "mcp:notion") || strings.Contains(probs, "mcp:ha") {
		t.Errorf("valid mcp scopes flagged:\n%s", probs)
	}
}

func TestAllowsMCP(t *testing.T) {
	c := &Character{Tools: []string{"mcp:notion/*", "mcp:ha/get_state"}}
	if !c.AllowsMCP("notion", "search") || !c.AllowsMCP("ha", "get_state") {
		t.Fatal("expected access")
	}
	if c.AllowsMCP("ha", "turn_on") || c.AllowsMCP("gmail", "send") {
		t.Fatal("unexpected access")
	}
}

func TestQuietHours(t *testing.T) {
	c := &Character{Initiative: Initiative{QuietHours: "22:00-08:00"}}
	if !c.InQuietHours(23*60) || !c.InQuietHours(7*60) || c.InQuietHours(12*60) {
		t.Fatal("overnight window wrong")
	}
	c.Initiative.QuietHours = "13:00-14:00"
	if !c.InQuietHours(13*60+30) || c.InQuietHours(14*60) {
		t.Fatal("daytime window wrong")
	}
}

func TestRoundTrip(t *testing.T) {
	c := &Character{ID: "pip", Name: "Pip", Role: "Keeps the calendar.", Personality: Personality{Traits: []string{"punctual"}},
		Voice: Voice{Style: []string{"brisk"}}, Tools: []string{"schedule"}, Body: "Pip is small and tidy."}
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Pip" || got.Body != "Pip is small and tidy." || !got.HasScope("schedule") {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestRegistryLoadSkipsBadFiles(t *testing.T) {
	r, dir := newReg(t)
	if err := r.EnsureDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nid: broken\n---\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "wrongname.md"), []byte("---\nid: other\nname: O\nrole: r\npersonality: {traits: [a]}\nvoice: {style: [b]}\n---\n"), 0o644)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if len(r.All()) != 1 {
		t.Fatalf("want only juniper, got %d", len(r.All()))
	}
	if len(r.Errors()) != 2 {
		t.Fatalf("want 2 errors, got %v", r.Errors())
	}
	if c, ok := r.FindByName("juni"); !ok || c.ID != "juniper" {
		t.Fatal("nickname lookup failed")
	}
	if r.Changed() {
		t.Fatal("nothing changed since load")
	}
	os.WriteFile(filepath.Join(dir, "new.md"), []byte("x"), 0o644)
	if !r.Changed() {
		t.Fatal("new file not detected")
	}
}

func TestWriteNeverClobbersUserEdits(t *testing.T) {
	r, dir := newReg(t)
	ctx := context.Background()
	c := &Character{ID: "pip", Name: "Pip", Role: "Keeps the calendar.", Personality: Personality{Traits: []string{"punctual"}}, Voice: Voice{Style: []string{"brisk"}}}
	if _, err := r.Write(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Thicket may update its own unedited file.
	c.Tools = []string{"schedule"}
	if _, err := r.Write(ctx, c); err != nil {
		t.Fatalf("rewrite of own file refused: %v", err)
	}
	// The user edits the file by hand.
	path := filepath.Join(dir, "pip.md")
	b, _ := os.ReadFile(path)
	edited := strings.Replace(string(b), "brisk", "brisk but kind", 1)
	os.WriteFile(path, []byte(edited), 0o644)

	c.Tools = nil
	proposed, err := r.Write(ctx, c)
	if !errors.Is(err, ErrUserEdited) {
		t.Fatalf("want ErrUserEdited, got %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatal("user edit was overwritten")
	}
	if _, err := os.Stat(proposed); err != nil || !strings.HasSuffix(proposed, ".proposed.md") {
		t.Fatalf("proposed file missing: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("pip"); !ok {
		t.Fatal("proposed file should not shadow the real one")
	}
}
