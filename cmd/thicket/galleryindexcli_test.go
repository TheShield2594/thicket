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
	"github.com/theshield2594/thicket/internal/gallery/gallerytest"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/websearch"
)

// The test gallery is served on this machine, which the real rules
// refuse: let the CLI reach it.
func allowLocalGallery(t *testing.T) {
	old := galleryFetcher
	galleryFetcher = func(*config.Config) *websearch.Fetcher { return &websearch.Fetcher{AllowPrivate: true} }
	t.Cleanup(func() { galleryFetcher = old })
}

func TestGalleryListShowInstall(t *testing.T) {
	allowLocalGallery(t)
	ctx := context.Background()
	k1, k2 := gallerytest.NewKey(t), gallerytest.NewKey(t)
	repo := gallerytest.Repo(t,
		gallerytest.Sprig{ID: "fern", Name: "Fern", Role: "Looks after your houseplants.", Tools: []string{"schedule"}, Key: k1,
			Skills: []store.Skill{{ID: "repot", Name: "Repot", Description: "Use when a plant outgrows its pot", Body: "1. pick a bigger pot"}}},
		gallerytest.Sprig{ID: "ledger", Name: "Ledger", Role: "Keeps the bills straight.", Key: k2})
	gallerytest.BuildIndex(t, repo, func(idx *sprigpkg.Index) {
		idx.Revoked = append(idx.Revoked, sprigpkg.IndexRevoked{KeyID: gallerytest.KeyID(k2), Reason: "lost"})
	})
	url := gallerytest.Serve(t, repo)

	d := t.TempDir()
	cfg := &config.Config{DataDir: d, CharactersDir: filepath.Join(d, "characters"), GalleryURL: url}
	db, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db.CreateUser(ctx, "web", "1", "Sam", "owner")
	db.Close()

	var out bytes.Buffer
	if err := galleryCmdIO(&config.Config{DataDir: d}, []string{"list"}, nil, &out); err == nil || !strings.Contains(err.Error(), "THICKET_GALLERY_URL") {
		t.Fatalf("no gallery set: %v", err)
	}
	if err := galleryCmdIO(cfg, []string{"list"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fern") || !strings.Contains(out.String(), "Looks after your houseplants.") || !strings.Contains(out.String(), "ledger") || !strings.Contains(out.String(), "revoked") {
		t.Fatalf("list:\n%s", out.String())
	}
	out.Reset()
	if err := galleryCmdIO(cfg, []string{"show", "fern"}, nil, &out); err != nil || !strings.Contains(out.String(), "Fingerprint: ") || !strings.Contains(out.String(), "Asks for: schedule") {
		t.Fatalf("show: %v\n%s", err, out.String())
	}

	// Install shows the normal preview and writes nothing on "n".
	out.Reset()
	if err := galleryCmdIO(cfg, []string{"install", "fern"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"matches the gallery index", "Signed by author key " + gallerytest.KeyID(k1), "fern/skills/repot.md.pending", "Nothing was written"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("install preview lacks %q:\n%s", s, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.CharactersDir, "fern.md")); err == nil {
		t.Fatal("written without a yes")
	}
	out.Reset()
	if err := galleryCmdIO(cfg, []string{"install", "fern"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.CharactersDir, "fern.md"))
	if err != nil {
		t.Fatalf("not installed:\n%s", out.String())
	}
	if c, err := character.Parse(b); err != nil || len(c.Tools) != 0 {
		t.Fatalf("installed with scopes: %+v %v", c, err)
	}

	// A revoked key's package is refused, by the gallery's list...
	if err := galleryCmdIO(cfg, []string{"install", "ledger", "--yes"}, nil, &out); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the gallery's revoked key: %v", err)
	}
	// ...also as a file...
	pkg := filepath.Join(t.TempDir(), "ledger.sprig")
	os.WriteFile(pkg, gallerytest.Package(t, gallerytest.Sprig{ID: "ledger", Name: "Ledger", Role: "Bills.", Key: k2}), 0o644)
	if err := importSprigCmd(cfg, []string{pkg, "--yes"}, nil, &out); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a file signed by a key the gallery revoked: %v", err)
	}
	out.Reset()
	if err := galleryCmdIO(cfg, []string{"revoked"}, nil, &out); err != nil || !strings.Contains(out.String(), gallerytest.KeyID(k2)+" (revoked by the gallery)") {
		t.Fatalf("revoked: %v %s", err, out.String())
	}
	// ...and by the local setting, on top of the gallery's.
	if err := galleryCmdIO(cfg, []string{"revoke", gallerytest.KeyID(k1)}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := galleryCmdIO(cfg, []string{"install", "fern", "--yes"}, nil, &out); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a locally revoked key: %v", err)
	}
}

func TestGalleryRepoCommands(t *testing.T) {
	k := gallerytest.NewKey(t)
	repo := gallerytest.Repo(t, gallerytest.Sprig{ID: "fern", Name: "Fern", Role: "Plants.", Key: k})
	var out bytes.Buffer
	if err := galleryCmdIO(&config.Config{}, []string{"verify", repo}, nil, &out); err != nil || !strings.Contains(out.String(), "1 sprig") {
		t.Fatalf("verify: %v %s", err, out.String())
	}
	os.WriteFile(filepath.Join(repo, "packages", "fern", "2.sprig"), []byte("not a package"), 0o644)
	out.Reset()
	if err := galleryCmdIO(&config.Config{}, []string{"verify", repo}, nil, &out); err == nil || !strings.Contains(out.String(), "2.sprig") {
		t.Fatalf("a bad package passed: %v %s", err, out.String())
	}
	if err := galleryCmdIO(&config.Config{}, []string{"build-index", repo}, nil, &out); err == nil {
		t.Fatal("an index was built over a bad package")
	}
	os.Remove(filepath.Join(repo, "packages", "fern", "2.sprig"))
	if err := galleryCmdIO(&config.Config{}, []string{"build-index", repo}, nil, &out); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(t.TempDir(), "site")
	if err := galleryCmdIO(&config.Config{}, []string{"build-site", repo, "-o", site}, nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "index.json", "packages/fern/1.sprig"} {
		if _, err := os.Stat(filepath.Join(site, f)); err != nil {
			t.Errorf("site lacks %s", f)
		}
	}
}
