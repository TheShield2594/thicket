package access

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// layout makes data/{thicket.db, characters/pip/, characters/pippa/,
// characters/bramble/secret.txt} and an outside folder, all resolved.
func layout(t *testing.T) (data, pip, outside string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data = filepath.Join(root, "data")
	for _, d := range []string{"characters/pip/notes", "characters/pippa", "characters/bramble"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"thicket.db", "characters/bramble/secret.txt", "characters/pip/notes/a.md"} {
		if err := os.WriteFile(filepath.Join(data, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside = filepath.Join(root, "shared")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	return data, filepath.Join(data, "characters", "pip"), outside
}

func TestPolicyRefusesEscapes(t *testing.T) {
	data, pip, outside := layout(t)
	// Links planted in the sprig's own folder.
	must(t, os.Symlink(filepath.Join(data, "thicket.db"), filepath.Join(pip, "db")))
	must(t, os.Symlink("../bramble", filepath.Join(pip, "friend")))
	must(t, os.Symlink(outside, filepath.Join(pip, "shared")))
	must(t, os.Symlink("loop", filepath.Join(pip, "loop")))
	p := Policy{Folder: pip, Protected: []string{data}}

	for _, path := range []string{
		"../bramble/secret.txt",
		"notes/../../bramble/secret.txt",
		"../../thicket.db",
		filepath.Join(data, "thicket.db"),
		filepath.Join(data, "characters", "bramble"),
		"db",                       // symlink to the DB
		"friend/secret.txt",        // symlink to another sprig
		"friend/../pip/../bramble", // through a link, then ".."
		"../pippa",                 // a prefix of pip's name isn't pip's
		"shared/x",                 // outside, not granted
		"/etc/passwd",
		"loop/x",
	} {
		if _, err := p.Check(path); err == nil {
			t.Errorf("%q was allowed", path)
		}
	}
	for _, path := range []string{"notes/a.md", "notes/new.md", "new/dir/file", ".", pip, "notes/../notes/a.md"} {
		if _, err := p.Check(path); err != nil {
			t.Errorf("%q refused: %v", path, err)
		}
	}
}

func TestPolicyGrants(t *testing.T) {
	data, pip, outside := layout(t)
	must(t, os.Symlink(outside, filepath.Join(pip, "shared")))
	p := Policy{Folder: pip, Grants: []string{outside}, Protected: []string{data}}
	got, err := p.Check("shared/report.csv")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(outside, "report.csv"); got != want {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
	if _, err := p.Check(filepath.Join(outside, "..", "data", "thicket.db")); !errors.Is(err, ErrOutside) {
		t.Fatalf("escaped a grant with ..: %v", err)
	}
	// A grant can't hold protected data, even if one was stored somehow.
	p.Grants = []string{filepath.Dir(data)}
	if _, err := p.Check(filepath.Join(data, "thicket.db")); err == nil {
		t.Fatal("a grant reached the database")
	}
}

func TestCheckGrant(t *testing.T) {
	data, _, outside := layout(t)
	prot := []string{data}
	for _, g := range []string{"/", filepath.Dir(data), data, filepath.Join(data, "characters", "bramble"), "relative/path"} {
		if _, err := CheckGrant(g, prot); err == nil {
			t.Errorf("grant %q accepted", g)
		}
	}
	// A link to the data folder resolves to it, so it's refused too.
	link := filepath.Join(outside, "sneaky")
	must(t, os.Symlink(data, link))
	if _, err := CheckGrant(link, prot); err == nil {
		t.Error("grant through a link to the data folder accepted")
	}
	if _, err := CheckGrant(filepath.Join(outside, "docs"), prot); err != nil {
		t.Errorf("ordinary grant refused: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
