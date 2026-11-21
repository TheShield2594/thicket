// Package gallery reads a shared sprig gallery: its index.json, fetched
// from THICKET_GALLERY_URL (off when empty) through the same public-address
// rules as web pages, cached in the data folder and refreshed daily, and
// its packages, each checked against the SHA-256 the index gives before
// the normal gallery import reads it (signature, hashes, every file).
//
// The index's revocation list is honoured on top of the local
// gallery.revoked_keys setting, wherever a gallery package is read.
package gallery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/websearch"
)

// MaxAge is how long a cached index is used before it's fetched again.
const MaxAge = 24 * time.Hour

// cacheName is the cache file in Client.Dir.
const cacheName = "gallery-index.json"

// Client reads one gallery. A nil *Client is a gallery that's off.
type Client struct {
	// URL is index.json's address (a URL ending in / means its folder).
	URL string
	// Dir holds the cached index (the data folder).
	Dir string
	// Fetcher downloads, refusing private addresses unless set otherwise
	// (tests). Nil means a default one.
	Fetcher *websearch.Fetcher
	Now     func() time.Time

	mu  sync.Mutex
	idx *sprigpkg.Index
	at  time.Time
}

// New returns the client for a gallery URL, or nil when it's empty.
func New(rawURL, dataDir string, f *websearch.Fetcher) *Client {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if strings.HasSuffix(rawURL, "/") {
		rawURL += sprigpkg.IndexFile
	}
	return &Client{URL: rawURL, Dir: dataDir, Fetcher: f}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) fetcher() *websearch.Fetcher {
	if c.Fetcher == nil {
		c.Fetcher = &websearch.Fetcher{}
	}
	return c.Fetcher
}

type cacheFile struct {
	URL       string          `json:"url"`
	FetchedAt time.Time       `json:"fetched_at"`
	Index     json.RawMessage `json:"index"`
}

// Load fetches and checks the index, without caching it.
func (c *Client) Load(ctx context.Context) (*sprigpkg.Index, []byte, error) {
	b, err := c.fetcher().Get(ctx, c.URL, sprigpkg.MaxIndexBytes)
	if errors.Is(err, websearch.ErrTooLarge) {
		return nil, nil, fmt.Errorf("the gallery index is too large (at most %d MB)", sprigpkg.MaxIndexBytes>>20)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't fetch the gallery index from %s: %w", c.URL, err)
	}
	idx, err := sprigpkg.ParseIndex(b)
	if err != nil {
		return nil, nil, err
	}
	return idx, b, nil
}

// Refresh fetches the index and caches it.
func (c *Client) Refresh(ctx context.Context) (*sprigpkg.Index, error) {
	idx, raw, err := c.Load(ctx)
	if err != nil {
		return nil, err
	}
	now := c.now()
	b, err := json.Marshal(cacheFile{URL: c.URL, FetchedAt: now, Index: raw})
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(c.Dir, cacheName), b); err != nil {
		return nil, fmt.Errorf("couldn't cache the gallery index: %w", err)
	}
	c.mu.Lock()
	c.idx, c.at = idx, now
	c.mu.Unlock()
	return idx, nil
}

// Cached is the cached index and when it was fetched, with no network.
// A cache made for another URL, or one that no longer checks out, is
// ignored. (nil, zero, nil) means there is none.
func (c *Client) Cached() (*sprigpkg.Index, time.Time, error) {
	if c == nil {
		return nil, time.Time{}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx != nil {
		return c.idx, c.at, nil
	}
	b, err := os.ReadFile(filepath.Join(c.Dir, cacheName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil || cf.URL != c.URL {
		return nil, time.Time{}, nil
	}
	idx, err := sprigpkg.ParseIndex(cf.Index)
	if err != nil {
		return nil, time.Time{}, nil
	}
	c.idx, c.at = idx, cf.FetchedAt
	return idx, cf.FetchedAt, nil
}

// ErrStale says the index is an old copy: fetching a new one failed.
var ErrStale = errors.New("using the gallery index cached earlier")

// Index is the cached index if it's less than a day old, else a fresh
// one. If fetching fails and there's an older copy, that copy comes back
// with an error wrapping ErrStale.
func (c *Client) Index(ctx context.Context) (*sprigpkg.Index, error) {
	idx, at, err := c.Cached()
	if err == nil && idx != nil && c.now().Sub(at) < MaxAge {
		return idx, nil
	}
	fresh, ferr := c.Refresh(ctx)
	if ferr == nil {
		return fresh, nil
	}
	if idx != nil {
		return idx, fmt.Errorf("%w (from %s): %v", ErrStale, at.Format("2 Jan 15:04"), ferr)
	}
	return nil, ferr
}

// Run refreshes the cached index once a day while ctx lasts.
func (c *Client) Run(ctx context.Context, warn func(error)) {
	for {
		_, at, _ := c.Cached()
		wait := time.Until(at.Add(MaxAge))
		if at.IsZero() || wait <= 0 {
			if _, err := c.Refresh(ctx); err != nil && ctx.Err() == nil && warn != nil {
				warn(err)
				wait = time.Hour // try again sooner after a failure
			} else {
				wait = MaxAge
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Revoked are the key ids the cached index revokes (no network).
func (c *Client) Revoked() []string {
	idx, _, _ := c.Cached()
	if idx == nil {
		return nil
	}
	return idx.RevokedIDs()
}

// RevokedKeys is every revoked author key id: the local setting
// gallery.revoked_keys, and the gallery index's list when there's one.
func RevokedKeys(ctx context.Context, db *store.Store, c *Client) ([]string, error) {
	list, err := sprigpkg.RevokedKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, k := range c.Revoked() {
		if !slices.Contains(list, k) {
			list = append(list, k)
		}
	}
	return list, nil
}

// Fetched is a package downloaded from the gallery and checked.
type Fetched struct {
	Raw     []byte
	Package sprigpkg.IndexPackage
	Version sprigpkg.IndexVersion
	Author  sprigpkg.IndexAuthor
	// File names it in previews: name-version.sprig.
	File string
}

// Fetch downloads one version (0: the newest) of a package and checks
// it: its author isn't revoked (here or by the index), its size and
// SHA-256 are the index's, it passes every gallery import check, and it's
// signed by the author key the index lists for it.
func (c *Client) Fetch(ctx context.Context, idx *sprigpkg.Index, name string, version int, revoked []string) (*Fetched, error) {
	p, ok := idx.Package(name)
	if !ok {
		return nil, fmt.Errorf("the gallery has no sprig called %q", name)
	}
	v, ok := p.Version(version)
	if !ok {
		return nil, fmt.Errorf("the gallery has no version %d of %s", version, p.Name)
	}
	author := idx.Authors[p.Author]
	gone := append(idx.RevokedIDs(), revoked...)
	if slices.Contains(gone, p.Author) {
		return nil, fmt.Errorf("%s's author key %s has been revoked, so it can't be installed", p.Name, p.Author)
	}
	base, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(v.File) // a relative path: the index's schema says so
	if err != nil || ref.IsAbs() || ref.Host != "" {
		return nil, errors.New("the index gives a bad file path")
	}
	raw, err := c.fetcher().Get(ctx, base.ResolveReference(ref).String(), sprigpkg.MaxPackageBytes)
	if errors.Is(err, websearch.ErrTooLarge) {
		return nil, fmt.Errorf("%s is larger than a gallery package may be", v.File)
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't download %s: %w", p.Name, err)
	}
	s := sha256.Sum256(raw)
	if int64(len(raw)) != v.Size || hex.EncodeToString(s[:]) != v.SHA256 {
		return nil, fmt.Errorf("the download of %s doesn't match its checksum in the gallery index, so it was refused", p.Name)
	}
	g, err := sprigpkg.ReadGallery(bytes.NewReader(raw), gone)
	if err != nil {
		return nil, fmt.Errorf("%s from the gallery can't be imported: %w", p.Name, err)
	}
	if g.Signer.KeyID != p.Author || g.Signer.EncodedKey() != author.PublicKey {
		return nil, fmt.Errorf("%s is signed by %s, not by the author key the gallery index lists (%s), so it was refused", p.Name, g.Signer.KeyID, p.Author)
	}
	if g.Manifest.ID != p.Name {
		return nil, fmt.Errorf("the gallery's %s is really the sprig %q, so it was refused", p.Name, g.Manifest.ID)
	}
	return &Fetched{Raw: raw, Package: *p, Version: v, Author: author, File: fmt.Sprintf("%s-%d.sprig", p.Name, v.Version)}, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gallery-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
