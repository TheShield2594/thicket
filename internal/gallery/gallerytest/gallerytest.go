// Package gallerytest builds gallery repositories and serves gallery
// sites for tests.
package gallerytest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/gallery"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

// Sprig is one package to put in a repository.
type Sprig struct {
	ID, Name, Role string
	Tools          []string
	Skills         []store.Skill
	Key            ed25519.PrivateKey
	Version        int // default 1
}

// NewKey makes an author key.
func NewKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	seed, _, err := sprigpkg.NewSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := sprigpkg.ParseSigningKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// KeyID is a private key's author key id.
func KeyID(k ed25519.PrivateKey) string { return sprigpkg.KeyID(k.Public().(ed25519.PublicKey)) }

// Package makes a signed gallery package.
func Package(t testing.TB, s Sprig) []byte {
	t.Helper()
	dir := t.TempDir()
	c := &character.Character{ID: s.ID, Name: s.Name, Role: s.Role, Tools: s.Tools,
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}}
	md, err := character.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, s.ID+".md"), md, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range s.Skills {
		s.Skills[i].SprigID = s.ID
	}
	g, err := sprigpkg.ExportGallery(sprigpkg.GallerySource{CharsDir: dir, ID: s.ID, Library: s.Skills, Version: "test", LooksSecret: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := g.Bytes(s.Key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Repo makes a gallery repository: every sprig's package, an author
// entry for each key, and index.json.
func Repo(t testing.TB, sprigs ...Sprig) string {
	t.Helper()
	dir := t.TempDir()
	authors := map[string]map[string]string{}
	for _, s := range sprigs {
		v := s.Version
		if v == 0 {
			v = 1
		}
		p := filepath.Join(dir, "packages", s.ID)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, itoa(v)+".sprig"), Package(t, s), 0o644); err != nil {
			t.Fatal(err)
		}
		pub := s.Key.Public().(ed25519.PublicKey)
		authors[KeyID(s.Key)] = map[string]string{"name": "Author " + KeyID(s.Key)[:4], "public_key": base64.StdEncoding.EncodeToString(pub)}
	}
	WriteJSON(t, filepath.Join(dir, "authors.json"), map[string]any{"authors": authors})
	BuildIndex(t, dir, nil)
	return dir
}

// BuildIndex writes index.json from the repository, then lets mutate
// change it (to make an index a real gallery wouldn't: one that revokes a
// key it still lists, say).
func BuildIndex(t testing.TB, dir string, mutate func(*sprigpkg.Index)) *sprigpkg.Index {
	t.Helper()
	idx, problems, err := sprigpkg.ScanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mutate == nil && len(problems) > 0 {
		t.Fatalf("repo problems: %v", problems)
	}
	if mutate != nil {
		mutate(idx)
	}
	b, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sprigpkg.IndexFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return idx
}

// Serve builds the repository's site and serves it; it returns the
// address of index.json. The server closes when the test ends.
func Serve(t testing.TB, repo string) string {
	t.Helper()
	site := t.TempDir()
	if err := gallery.BuildSite(repo, site); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(site)))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + sprigpkg.IndexFile
}

// WriteJSON writes v as JSON.
func WriteJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
