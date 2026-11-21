package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/gallery/gallerytest"
	"github.com/theshield2594/thicket/internal/websearch"
)

func TestDoctorGallery(t *testing.T) {
	repo := gallerytest.Repo(t, gallerytest.Sprig{ID: "fern", Name: "Fern", Role: "Plants.", Key: gallerytest.NewKey(t)})
	url := gallerytest.Serve(t, repo)
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CharactersDir: filepath.Join(dir, "characters"), ModelURL: "http://127.0.0.1:1/v1", HTTPAddr: ":0", GalleryURL: url}
	run := func(d *Doctor) Check {
		t.Helper()
		for _, c := range d.Run(context.Background()) {
			if c.Name == "gallery index" {
				return c
			}
		}
		t.Fatal("no gallery check")
		return Check{}
	}
	// The real rules refuse this machine's addresses, as Thicket does.
	if c := run(&Doctor{Cfg: cfg, ServerURL: "http://127.0.0.1:1"}); c.OK || !c.Optional || !strings.Contains(c.Detail, "private network") {
		t.Fatalf("blocked: %+v", c)
	}
	local := &websearch.Fetcher{AllowPrivate: true}
	if c := run(&Doctor{Cfg: cfg, ServerURL: "http://127.0.0.1:1", GalleryFetcher: local}); !c.OK || !strings.Contains(c.Detail, "schema valid, 1 sprigs") || !strings.Contains(c.Detail, "not cached yet") {
		t.Fatalf("gallery: %+v", c)
	}
	cfg.GalleryURL = strings.TrimSuffix(url, "index.json") + "packages/fern/1.sprig" // not an index
	if c := run(&Doctor{Cfg: cfg, ServerURL: "http://127.0.0.1:1", GalleryFetcher: local}); c.OK {
		t.Fatalf("a bad index passed: %+v", c)
	}
}
