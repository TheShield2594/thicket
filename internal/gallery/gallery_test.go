package gallery_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/gallery"
	"github.com/theshield2594/thicket/internal/gallery/gallerytest"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/websearch"
)

func TestClientFetchesCachesAndChecks(t *testing.T) {
	ctx := context.Background()
	k1, k2 := gallerytest.NewKey(t), gallerytest.NewKey(t)
	repo := gallerytest.Repo(t,
		gallerytest.Sprig{ID: "fern", Name: "Fern", Role: "Looks after your houseplants.", Key: k1},
		gallerytest.Sprig{ID: "ledger", Name: "Ledger", Role: "Bills.", Key: k2})
	// The gallery revokes k2 while still listing its sprig.
	gallerytest.BuildIndex(t, repo, func(idx *sprigpkg.Index) {
		idx.Revoked = append(idx.Revoked, sprigpkg.IndexRevoked{KeyID: gallerytest.KeyID(k2), Reason: "stolen"})
	})
	site := t.TempDir()
	if err := gallery.BuildSite(repo, site); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	var tamper atomic.Bool
	files := http.FileServer(http.Dir(site))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			hits.Add(1)
		}
		if tamper.Load() && strings.HasSuffix(r.URL.Path, ".sprig") {
			b, _ := os.ReadFile(filepath.Join(site, filepath.FromSlash(r.URL.Path)))
			b[len(b)-10] ^= 1
			w.Write(b)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer srv.Close()

	// The normal rules refuse this machine's addresses.
	dataDir := t.TempDir()
	if _, err := gallery.New(srv.URL+"/", dataDir, nil).Index(ctx); err == nil || !errors.Is(err, websearch.ErrBlocked) {
		t.Fatalf("a loopback gallery was fetched with the default rules: %v", err)
	}

	now := time.Now()
	c := gallery.New(srv.URL+"/", dataDir, &websearch.Fetcher{AllowPrivate: true})
	c.Now = func() time.Time { return now }
	idx, err := c.Index(ctx)
	if err != nil || len(idx.Packages) != 2 {
		t.Fatalf("index: %v %+v", err, idx)
	}
	if _, err := c.Index(ctx); err != nil || hits.Load() != 1 {
		t.Fatalf("a fresh cache was fetched again: %d fetches", hits.Load())
	}
	// A new client reads the cache from disk.
	c2 := gallery.New(srv.URL+"/", dataDir, &websearch.Fetcher{AllowPrivate: true})
	if cached, at, err := c2.Cached(); err != nil || cached == nil || at.IsZero() || len(c2.Revoked()) != 1 {
		t.Fatalf("cache: %v %v", err, cached)
	}
	// A day later it's fetched again; if that fails, the old copy is used.
	now = now.Add(25 * time.Hour)
	if _, err := c.Index(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("a day-old cache wasn't refreshed: %v %d", err, hits.Load())
	}
	now = now.Add(25 * time.Hour)
	c.URL = srv.URL + "/missing.json"
	if old, err := c.Index(ctx); old == nil || !errors.Is(err, gallery.ErrStale) {
		t.Fatalf("a failed refresh should give the old copy: %v", err)
	}
	c.URL = srv.URL + "/index.json"

	f, err := c.Fetch(ctx, idx, "fern", 0, nil)
	if err != nil || f.Version.Version != 1 || f.File != "fern-1.sprig" {
		t.Fatalf("fetch: %v %+v", err, f)
	}
	if _, err := c.Fetch(ctx, idx, "ledger", 0, nil); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the index's revocation wasn't honoured: %v", err)
	}
	if _, err := c.Fetch(ctx, idx, "fern", 0, []string{gallerytest.KeyID(k1)}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a local revocation wasn't honoured: %v", err)
	}
	tamper.Store(true)
	if _, err := c.Fetch(ctx, idx, "fern", 0, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a changed download was accepted: %v", err)
	}
	tamper.Store(false)
	if _, err := c.Fetch(ctx, idx, "nope", 0, nil); err == nil {
		t.Fatal("an unknown sprig")
	}

	// The site lists every sprig with its author and fingerprint.
	html, _ := os.ReadFile(filepath.Join(site, "index.html"))
	for _, want := range []string{"Fern", "Looks after your houseplants.", idx.Authors[gallerytest.KeyID(k1)].Fingerprint, "packages/fern/1.sprig", "<svg", "revoked"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("site lacks %q", want)
		}
	}
}

func TestSiteEscapesStrangersText(t *testing.T) {
	k := gallerytest.NewKey(t)
	repo := gallerytest.Repo(t, gallerytest.Sprig{ID: "fern", Name: "Fern", Role: `Plants <script>alert(1)</script>`, Key: k})
	site := t.TempDir()
	if err := gallery.BuildSite(repo, site); err != nil {
		t.Fatal(err)
	}
	html, _ := os.ReadFile(filepath.Join(site, "index.html"))
	if strings.Contains(string(html), "<script>alert") {
		t.Fatal("a description's markup reached the page")
	}
}
