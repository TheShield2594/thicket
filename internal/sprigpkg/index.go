package sprigpkg

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/theshield2594/thicket/internal/avatar"
)

// The gallery index (index.json) lists what a shared gallery holds: every
// package with its versions and their SHA-256, the authors' public keys,
// and the author keys the moderators revoked. It is made from a gallery
// repository with `thicket gallery build-index`, never by hand, and
// `thicket gallery verify` refuses a repository whose index.json isn't
// exactly what its packages make.
//
// A gallery repository holds:
//
//	authors.json        the authors moderators accepted: key id → name, public key
//	revoked.json        revoked author keys, with a reason and date
//	packages/ID/N.sprig version N of sprig ID (a signed gallery package)
//	index.json          made from the above
//
// Names and descriptions in the index are written by strangers: Thicket
// shows them as data, and a package is checked in full (signature, hashes,
// every file) when it is downloaded, as any gallery import is.

const (
	IndexKind   = "thicket-gallery-index"
	IndexFormat = 1
	// MaxIndexBytes caps index.json.
	MaxIndexBytes = 4 << 20
	// MaxPackageBytes caps one package file in a gallery.
	MaxPackageBytes = 4 << 20
	// IndexFile is the index's name in a gallery repository and site.
	IndexFile = "index.json"
)

// Index is index.json.
type Index struct {
	Kind      string                 `json:"kind"`
	Format    int                    `json:"format"`
	UpdatedAt string                 `json:"updated_at"`
	Authors   map[string]IndexAuthor `json:"authors"`
	Packages  []IndexPackage         `json:"packages"`
	Revoked   []IndexRevoked         `json:"revoked"`
}

// IndexAuthor is an author key the moderators accepted.
type IndexAuthor struct {
	KeyID       string `json:"key_id"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
}

// IndexPackage is one sprig in the gallery. Name is its id.
type IndexPackage struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"display_name"`
	Description string         `json:"description"`
	Author      string         `json:"author"`
	Scopes      []string       `json:"scopes"`
	Avatar      avatar.Params  `json:"avatar"`
	Versions    []IndexVersion `json:"versions"`
}

// IndexVersion is one version's file and checksum.
type IndexVersion struct {
	Version        int    `json:"version"`
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	CreatedAt      string `json:"created_at"`
	ThicketVersion string `json:"thicket_version,omitempty"`
}

// IndexRevoked is a revoked author key.
type IndexRevoked struct {
	KeyID     string `json:"key_id"`
	Reason    string `json:"reason,omitempty"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// ParseIndex reads and checks index.json: its size, its schema (closed
// objects), and that it agrees with itself: every key id is its key's,
// every package's author is listed, every version's file is where it
// should be.
func ParseIndex(b []byte) (*Index, error) {
	if len(b) > MaxIndexBytes {
		return nil, fmt.Errorf("the gallery index is too large (at most %d MB)", MaxIndexBytes>>20)
	}
	if err := checkText(b); err != nil {
		return nil, fmt.Errorf("the gallery index %w", err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("the gallery index can't be read: %w", err)
	}
	if obj, ok := raw.(map[string]any); ok {
		if f, ok := obj["format"].(float64); ok && f > IndexFormat {
			return nil, fmt.Errorf("the gallery index is format %v; this Thicket reads format %d. Update Thicket to use it", f, IndexFormat)
		}
	}
	if err := loadSchema("index").validate(raw, "index.json"); err != nil {
		return nil, err
	}
	var idx Index
	if err := strictJSON(b, &idx); err != nil {
		return nil, fmt.Errorf("the gallery index can't be read: %w", err)
	}
	if err := idx.check(); err != nil {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	return &idx, nil
}

func (idx *Index) check() error {
	for k, a := range idx.Authors {
		pub, err := decodeKey(a.PublicKey)
		if err != nil {
			return fmt.Errorf("author %s: %w", clip(k), err)
		}
		if k != a.KeyID || KeyID(pub) != k {
			return fmt.Errorf("author %s: the key id isn't that public key's", clip(k))
		}
		if Fingerprint(pub) != a.Fingerprint {
			return fmt.Errorf("author %s: the fingerprint isn't that public key's", k)
		}
	}
	seen := map[string]bool{}
	for _, p := range idx.Packages {
		if seen[p.Name] {
			return fmt.Errorf("%s is listed twice", p.Name)
		}
		seen[p.Name] = true
		if _, ok := idx.Authors[p.Author]; !ok {
			return fmt.Errorf("%s: its author key %s isn't listed", p.Name, p.Author)
		}
		if err := p.Avatar.Validate(); err != nil {
			return fmt.Errorf("%s: avatar: %w", p.Name, err)
		}
		if len(p.Versions) == 0 {
			return fmt.Errorf("%s has no versions", p.Name)
		}
		for i, v := range p.Versions {
			if i > 0 && v.Version <= p.Versions[i-1].Version {
				return fmt.Errorf("%s: versions must be listed once each, oldest first", p.Name)
			}
			if v.File != versionFile(p.Name, v.Version) {
				return fmt.Errorf("%s version %d: its file must be %s", p.Name, v.Version, versionFile(p.Name, v.Version))
			}
		}
	}
	return nil
}

func versionFile(name string, v int) string { return fmt.Sprintf("packages/%s/%d.sprig", name, v) }

func decodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("the public key isn't an Ed25519 key")
	}
	return ed25519.PublicKey(b), nil
}

// Package finds a package by name.
func (idx *Index) Package(name string) (*IndexPackage, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range idx.Packages {
		if idx.Packages[i].Name == name {
			return &idx.Packages[i], true
		}
	}
	return nil, false
}

// RevokedIDs are the key ids the index revokes.
func (idx *Index) RevokedIDs() []string {
	out := make([]string, 0, len(idx.Revoked))
	for _, r := range idx.Revoked {
		out = append(out, r.KeyID)
	}
	return out
}

// Latest is a package's newest version.
func (p *IndexPackage) Latest() IndexVersion { return p.Versions[len(p.Versions)-1] }

// Version finds one version; 0 means the newest.
func (p *IndexPackage) Version(n int) (IndexVersion, bool) {
	if n == 0 {
		return p.Latest(), true
	}
	for _, v := range p.Versions {
		if v.Version == n {
			return v, true
		}
	}
	return IndexVersion{}, false
}

// stopWords are left out of a search.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "who": true, "that": true, "with": true, "want": true, "someone": true,
	"something": true, "sprig": true, "help": true, "helps": true, "need": true, "would": true, "like": true,
	"can": true, "you": true, "your": true, "this": true, "from": true, "keep": true, "keeps": true, "about": true,
	// Verbs any need might use: they say little about which sprig fits.
	"track": true, "tracks": true, "tracking": true, "manage": true, "manages": true, "remind": true, "reminds": true,
	"look": true, "looks": true, "after": true, "take": true, "care": true, "make": true, "sure": true, "get": true,
	"know": true, "my": true, "our": true, "all": true, "things": true,
}

func searchWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 3 || stopWords[w] {
			continue
		}
		w = stem(w)
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// stem drops a plural or -ing ending, so "plants" finds "plant".
func stem(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		return w[:len(w)-3]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 3:
		return w[:len(w)-1]
	}
	return w
}

// Search finds packages matching words of query in their name or
// description, best first, at most n. Packages by a key in revoked (or
// revoked by the index) are left out.
func (idx *Index) Search(query string, n int, revoked []string) []IndexPackage {
	words := searchWords(query)
	if len(words) == 0 || n <= 0 {
		return nil
	}
	gone := append(idx.RevokedIDs(), revoked...)
	type hit struct {
		p     IndexPackage
		score int
	}
	var hits []hit
	for _, p := range idx.Packages {
		if slices.Contains(gone, p.Author) {
			continue
		}
		title := searchWords(p.Name + " " + p.DisplayName)
		desc := searchWords(p.Description)
		score := 0
		for _, w := range words {
			switch {
			case slices.Contains(title, w):
				score += 3
			case slices.Contains(desc, w):
				score += 2
			case len(w) >= 4 && strings.Contains(strings.ToLower(p.DisplayName+" "+p.Description), w):
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{p, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].p.Name < hits[j].p.Name
	})
	var out []IndexPackage
	for i := 0; i < len(hits) && i < n; i++ {
		out = append(out, hits[i].p)
	}
	return out
}

// Marshal is index.json's text, as build-index writes it.
func (idx *Index) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// --- a gallery repository ---

var (
	repoVersionFile = regexp.MustCompile(`^([1-9][0-9]{0,5})\.sprig$`)
	repoPackageDir  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// repoTopLevel are the entries a repository's root may hold beside
	// the gallery's own (dot files, docs and the generated site).
	repoTopLevel = regexp.MustCompile(`^(\..*|README\.md|LICENSE.*|CONTRIBUTING\.md|site)$`)
)

type repoAuthors struct {
	Authors map[string]struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
		URL       string `json:"url,omitempty"`
	} `json:"authors"`
}

type repoRevoked struct {
	Revoked []IndexRevoked `json:"revoked"`
}

// readRepoJSON reads an optional moderator file against its schema.
func readRepoJSON(path, schemaName string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := checkText(b); err != nil {
		return true, fmt.Errorf("%s %w", filepath.Base(path), err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return true, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := loadSchema(schemaName).validate(raw, filepath.Base(path)); err != nil {
		return true, err
	}
	if err := strictJSON(b, v); err != nil {
		return true, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// ScanRepo reads a gallery repository and builds the index it should
// have. Every package is read with import's checks (the signature, every
// hash, the file allowlist, every schema) and must be signed by a key in
// authors.json that isn't revoked, the same key for every version of a
// sprig. problems lists every check that failed; err is for a repository
// that can't be read at all.
func ScanRepo(dir string) (idx *Index, problems []string, err error) {
	problem := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	var authors repoAuthors
	var revoked repoRevoked
	hasAuthors, aerr := readRepoJSON(filepath.Join(dir, "authors.json"), "authors", &authors)
	if aerr != nil {
		problem("%v", aerr)
	}
	if _, rerr := readRepoJSON(filepath.Join(dir, "revoked.json"), "revoked", &revoked); rerr != nil {
		problem("%v", rerr)
	}
	idx = &Index{Kind: IndexKind, Format: IndexFormat, Authors: map[string]IndexAuthor{}, Packages: []IndexPackage{}, Revoked: []IndexRevoked{}}
	latest := ""
	later := func(s string) {
		if s > latest {
			latest = s
		}
	}
	keys := make([]string, 0, len(authors.Authors))
	for k := range authors.Authors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := authors.Authors[k]
		pub, err := decodeKey(a.PublicKey)
		if err != nil {
			problem("authors.json: %s: %v", clip(k), err)
			continue
		}
		if KeyID(pub) != k {
			problem("authors.json: %s isn't the key id of that public key (it's %s)", clip(k), KeyID(pub))
			continue
		}
		idx.Authors[k] = IndexAuthor{KeyID: k, PublicKey: a.PublicKey, Fingerprint: Fingerprint(pub), Name: a.Name, URL: a.URL}
	}
	gone := map[string]bool{}
	for _, r := range revoked.Revoked {
		if gone[r.KeyID] {
			problem("revoked.json lists %s twice", r.KeyID)
			continue
		}
		gone[r.KeyID] = true
		idx.Revoked = append(idx.Revoked, r)
		later(r.RevokedAt)
	}
	sort.Slice(idx.Revoked, func(i, j int) bool { return idx.Revoked[i].KeyID < idx.Revoked[j].KeyID })

	top, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range top {
		switch n := e.Name(); n {
		case "authors.json", "revoked.json", IndexFile, "packages":
		default:
			if !repoTopLevel.MatchString(n) {
				problem("%s doesn't belong in a gallery repository (only authors.json, revoked.json, index.json, packages/, site/ and docs)", n)
			}
		}
	}
	pkgDir := filepath.Join(dir, "packages")
	fi, err := os.Lstat(pkgDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fi = nil
	case err != nil:
		return nil, nil, err
	case !fi.IsDir():
		problem("packages must be a folder")
		fi = nil
	}
	if fi != nil {
		ents, err := os.ReadDir(pkgDir)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range ents {
			if e.Name() == ".gitkeep" && e.Type().IsRegular() {
				continue // keeps an empty packages/ in git
			}
			if e.Type()&os.ModeSymlink != 0 || !e.IsDir() || !repoPackageDir.MatchString(e.Name()) {
				problem("packages/%s: only a folder per sprig, named by its id, belongs in packages/", e.Name())
				continue
			}
			if p, ok := scanPackage(filepath.Join(pkgDir, e.Name()), e.Name(), idx.Authors, gone, hasAuthors, problem, later); ok {
				idx.Packages = append(idx.Packages, p)
			}
		}
	}
	sort.Slice(idx.Packages, func(i, j int) bool { return idx.Packages[i].Name < idx.Packages[j].Name })
	idx.UpdatedAt = latest
	return idx, problems, nil
}

// scanPackage reads every version of one sprig.
func scanPackage(dir, id string, authors map[string]IndexAuthor, gone map[string]bool, hasAuthors bool, problem func(string, ...any), later func(string)) (IndexPackage, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		problem("packages/%s: %v", id, err)
		return IndexPackage{}, false
	}
	type ver struct {
		n   int
		raw []byte
		g   *Gallery
	}
	var vers []ver
	bad := false
	for _, e := range ents {
		rel := "packages/" + id + "/" + e.Name()
		m := repoVersionFile.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			problem("%s: a sprig's folder holds only its versions, as plain files named 1.sprig, 2.sprig…", rel)
			bad = true
			continue
		}
		n, _ := strconv.Atoi(m[1])
		info, err := e.Info()
		if err != nil {
			problem("%s: %v", rel, err)
			bad = true
			continue
		}
		if info.Size() > MaxPackageBytes {
			problem("%s is too large (at most %d MB)", rel, MaxPackageBytes>>20)
			bad = true
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			problem("%s: %v", rel, err)
			bad = true
			continue
		}
		g, err := ReadGallery(bytes.NewReader(raw), nil)
		if err != nil {
			problem("%s: %v", rel, err)
			bad = true
			continue
		}
		if g.Manifest.ID != id {
			problem("%s is the sprig %q, so it belongs in packages/%s/", rel, g.Manifest.ID, g.Manifest.ID)
			bad = true
			continue
		}
		k := g.Signer.KeyID
		switch a, ok := authors[k]; {
		case gone[k]:
			problem("%s is signed by %s, a revoked key: remove it", rel, k)
			bad = true
			continue
		case !ok:
			hint := ""
			if !hasAuthors {
				hint = " (there's no authors.json yet)"
			}
			problem("%s is signed by %s, which isn't in authors.json%s. If the author checks out (compare fingerprint %s with the one they publish), add \"%s\": {\"name\": \"…\", \"public_key\": \"%s\"}",
				rel, k, hint, g.Signer.Fingerprint(), k, g.Signer.EncodedKey())
			bad = true
			continue
		case a.PublicKey != g.Signer.EncodedKey():
			problem("%s is signed by a different public key than authors.json has for %s", rel, k)
			bad = true
			continue
		}
		vers = append(vers, ver{n, raw, g})
	}
	if bad || len(vers) == 0 {
		if len(vers) == 0 && !bad {
			problem("packages/%s is empty", id)
		}
		return IndexPackage{}, false
	}
	sort.Slice(vers, func(i, j int) bool { return vers[i].n < vers[j].n })
	key := vers[0].g.Signer.KeyID
	for _, v := range vers[1:] {
		if v.g.Signer.KeyID != key {
			problem("%s: version %d is signed by %s, but version %d by %s; every version of a sprig must be signed by the same key", versionFile(id, v.n), v.n, v.g.Signer.KeyID, vers[0].n, key)
			return IndexPackage{}, false
		}
	}
	newest := vers[len(vers)-1].g
	c := newest.Char
	av := avatar.Default(id, c.Color)
	if b, ok := newest.Files["avatar.json"]; ok {
		var p avatar.Params
		if json.Unmarshal(b, &p) == nil {
			if m := p.Merge(av); m.Validate() == nil {
				av = m
			}
		}
	}
	p := IndexPackage{Name: id, DisplayName: c.Name, Description: c.Role, Author: key, Scopes: append([]string{}, c.Tools...), Avatar: av}
	for _, v := range vers {
		created := v.g.Manifest.CreatedAt.UTC().Format(time.RFC3339)
		later(created)
		p.Versions = append(p.Versions, IndexVersion{Version: v.n, File: versionFile(id, v.n), SHA256: sum(v.raw), Size: int64(len(v.raw)),
			CreatedAt: created, ThicketVersion: v.g.Manifest.ThicketVersion})
	}
	return p, true
}

// VerifyRepo checks a gallery repository as its CI does: every package
// (ScanRepo), and an index.json that is exactly what build-index makes.
// It returns every problem; none means the change may be merged.
func VerifyRepo(dir string) ([]string, error) {
	idx, problems, err := ScanRepo(dir)
	if err != nil {
		return nil, err
	}
	want, err := idx.Marshal()
	if err != nil {
		return nil, err
	}
	have, err := os.ReadFile(filepath.Join(dir, IndexFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		problems = append(problems, "there's no index.json: run thicket gallery build-index")
	case err != nil:
		return nil, err
	case !bytes.Equal(have, want):
		if _, perr := ParseIndex(have); perr != nil {
			problems = append(problems, perr.Error())
		}
		problems = append(problems, "index.json isn't what the packages make (it was edited by hand, or not rebuilt): run thicket gallery build-index")
	}
	if len(problems) == 0 {
		// What CI accepts, Thicket must be able to read.
		if _, err := ParseIndex(want); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems, nil
}
