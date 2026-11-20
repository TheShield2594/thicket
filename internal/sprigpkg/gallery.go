package sprigpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"

	"gopkg.in/yaml.v3"
)

// Gallery packages (`thicket export-sprig ID --for-gallery`) are sprig
// packages meant for strangers, so they carry much less and are checked
// much harder than ordinary ones:
//
//   - Only character.md, sprig/avatar.json, sprig/voice_tests/*.yaml,
//     skills/*.md, manifest.json and signature. No routines, memories,
//     pictures, or anything tied to the install that made it.
//   - signature is a detached Ed25519 signature over manifest.json, with
//     the author's public key and key id. Import verifies it before
//     reading anything else, then every file's SHA-256 from the manifest.
//   - Every file must be UTF-8 Markdown, YAML or JSON within size limits,
//     and pass its JSON Schema (schemas/). Links, "..", absolute paths and
//     MCP server or integration definitions are refused wherever they are.
//   - The author's key is trusted on first use; an update of a sprig must
//     be signed by the key it first came with; revoked keys are refused.
//   - The imported sprig is granted no scopes (the ones it asks for are
//     listed), every tool starts at draft, and its skills arrive as its
//     own but inactive until someone who may manage it approves each.

const (
	// GalleryKind and GalleryFormat identify a gallery package.
	GalleryKind   = "thicket-gallery-sprig"
	GalleryFormat = 1
	// GallerySecret is the stored secret holding the author's signing
	// key (the Ed25519 seed, base64).
	GallerySecret = "gallery_signing_key"

	signatureName       = "signature"
	skillsDirPrefix     = "skills/"
	galleryMaxFileBytes = 256 << 10
	galleryMaxTotal     = 2 << 20
	galleryMaxFiles     = 100
	galleryMaxSkills    = 20
	galleryMaxSkillBody = 8000
	// PendingSuffix marks a sprig's skill waiting for approval: the skill
	// loader only reads *.md files, so it's inactive until renamed.
	PendingSuffix = ".pending"
)

// galleryPath is every path a gallery package may hold.
var galleryPath = regexp.MustCompile(`^(manifest\.json|signature|character\.md|sprig/avatar\.json|sprig/voice_tests/[A-Za-z0-9][A-Za-z0-9._-]{0,80}\.yaml|skills/[a-z][a-z0-9-]{1,39}\.md)$`)

// galleryScope is a tool entry a gallery sprig may ask for: a whole scope,
// never a single integration tool.
var galleryScope = regexp.MustCompile(`^(schedule|web|calendar|characters|admin|mcp:[a-z0-9][a-z0-9_-]{0,40}/\*)$`)

// mcpDefinition finds MCP server or integration definitions (Claude-style
// "mcpServers", "mcp_servers:", "integrations:" blocks, or server launch
// settings), wherever they are in a file.
var mcpDefinition = regexp.MustCompile(`(?i)(["']?\bmcp[_-]?servers?["']?\s*[:=]|["']?\bintegrations?["']?\s*[:=]\s*[\[{\n]|["']\b(command|transport|server_url)["']\s*:)`)

// KeyID is the short id of an author key: the first 16 hex characters of
// the SHA-256 of the public key.
func KeyID(pub ed25519.PublicKey) string {
	s := sha256.Sum256(pub)
	return hex.EncodeToString(s[:8])
}

// Fingerprint is the full SHA-256 of an author's public key, grouped for
// reading aloud or comparing by eye.
func Fingerprint(pub ed25519.PublicKey) string {
	s := sha256.Sum256(pub)
	h := hex.EncodeToString(s[:])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, " ")
}

// NewSigningKey makes an author key pair. The returned seed (base64) is
// what's stored; the private key is derived from it.
func NewSigningKey() (seed string, pub ed25519.PublicKey, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), pub, nil
}

// ParseSigningKey turns a stored seed back into the private key.
func ParseSigningKey(seed string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seed))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("the stored gallery signing key isn't valid; make a new one with thicket gallery keygen")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// GalleryManifest describes a gallery package; Files holds each file's
// SHA-256 (manifest.json and signature aren't in it).
type GalleryManifest struct {
	Kind           string            `json:"kind"`
	Format         int               `json:"format"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	ThicketVersion string            `json:"thicket_version,omitempty"`
	AuthorKey      string            `json:"author_key"`
	CreatedAt      time.Time         `json:"created_at"`
	Files          map[string]string `json:"files"`
}

// Signature is the signature file.
type Signature struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// Signer is who signed a package.
type Signer struct {
	KeyID     string
	PublicKey ed25519.PublicKey
}

// Fingerprint is the signer's key fingerprint.
func (s Signer) Fingerprint() string { return Fingerprint(s.PublicKey) }

// EncodedKey is the signer's public key, base64.
func (s Signer) EncodedKey() string { return base64.StdEncoding.EncodeToString(s.PublicKey) }

// Gallery is a gallery package in memory.
type Gallery struct {
	Manifest GalleryManifest
	Signer   Signer
	Char     *character.Character
	// Files are from the sprig's folder, relative to it: avatar.json and
	// voice_tests/*.yaml.
	Files map[string][]byte
	// Skills become the sprig's own skills (inactive until approved).
	Skills []store.Skill
	// Asks are the scopes the sprig asks for: none are granted on import.
	Asks []string
	// Omitted says what export left out and why.
	Omitted []string
	// Skipped are package skills not installed (an id already used here),
	// with why; set by PlanSkills. SkippedSkills are those skills, so the
	// preview still shows every file in full.
	Skipped       []string
	SkippedSkills []store.Skill
}

// SkillIDs are the ids of the skills that will be installed, in order:
// what an approval was shown.
func (g *Gallery) SkillIDs() []string {
	out := make([]string, 0, len(g.Skills))
	for _, k := range g.Skills {
		out = append(out, k.ID)
	}
	return out
}

// GallerySource is what ExportGallery reads.
type GallerySource struct {
	CharsDir string
	ID       string
	// Library is the skill library: the sprig's own skills go in, and the
	// shared skills it mentions (or ExtraSkills) go in as its own.
	Library     []store.Skill
	ExtraSkills []string
	Version     string
	// LooksSecret reports secret-shaped text. Required.
	LooksSecret func(string) bool
	Now         time.Time
}

// ExportGallery builds a gallery package for one sprig. It refuses (and
// writes nothing) if a tool entry isn't a whole scope, if anything looks
// like a secret, or if the result wouldn't pass import's checks.
func ExportGallery(src GallerySource) (*Gallery, error) {
	if src.LooksSecret == nil {
		return nil, errors.New("sprigpkg: LooksSecret is required")
	}
	b, err := os.ReadFile(filepath.Join(src.CharsDir, filepath.Base(src.ID)+".md"))
	if err != nil {
		return nil, fmt.Errorf("no sprig %q: %w", src.ID, err)
	}
	c, err := character.Parse(b)
	if err != nil {
		return nil, err
	}
	if c.IsKid() {
		return nil, fmt.Errorf("%s is a kid sprig, which belongs to this household's kids and parents and can't go to the gallery", c.DisplayName())
	}
	for _, t := range c.Tools {
		if !galleryScope.MatchString(t) {
			return nil, fmt.Errorf("not exported for the gallery: %s's tools name %q, which isn't a whole scope. A gallery sprig asks for scopes (schedule, web, calendar, characters, admin or mcp:<server>/*), never single tools", c.DisplayName(), t)
		}
	}
	g := &Gallery{Char: c, Files: map[string][]byte{}}
	if c.Avatar != "" {
		g.Omitted = append(g.Omitted, "the picture "+c.Avatar+" (gallery packages carry the drawn avatar, sprig/avatar.json, only)")
	}
	if c.Model != "" {
		g.Omitted = append(g.Omitted, "the model setting "+c.Model+" (the importer's own model is used)")
	}
	// What belongs to this install stays here.
	c.BelongsTo, c.Kids, c.Parents, c.Allow, c.Limits = 0, nil, nil, nil, character.KidLimits{}
	c.Host, c.CreatedBy, c.Household, c.Avatar, c.Model = false, "", "", "", ""
	dir := filepath.Join(src.CharsDir, c.ID)
	if b, err := os.ReadFile(filepath.Join(dir, "avatar.json")); err == nil {
		g.Files["avatar.json"] = b
	}
	tests, _ := filepath.Glob(filepath.Join(dir, "voice_tests", "*.yaml"))
	for _, f := range tests {
		rel := "voice_tests/" + filepath.Base(f)
		if !galleryPath.MatchString(sprigDirPrefix + rel) {
			g.Omitted = append(g.Omitted, "the voice test "+filepath.Base(f)+" (its file name has unusual characters)")
			continue
		}
		if b, err := os.ReadFile(f); err == nil {
			g.Files[rel] = b
		}
	}
	var shared []store.Skill
	seen := map[string]bool{}
	for _, k := range src.Library {
		switch {
		case k.SprigID == c.ID:
			g.Skills = append(g.Skills, k)
			seen[k.ID] = true
		case k.Shared():
			shared = append(shared, k)
		}
	}
	for _, k := range usedSkills(c, nil, shared, src.ExtraSkills) {
		if !seen[k.ID] {
			g.Skills = append(g.Skills, k.Store())
			seen[k.ID] = true
		}
	}
	sort.Slice(g.Skills, func(i, j int) bool { return g.Skills[i].ID < g.Skills[j].ID })
	if len(g.Skills) > galleryMaxSkills {
		return nil, fmt.Errorf("%s has %d skills; a gallery package holds at most %d", c.DisplayName(), len(g.Skills), galleryMaxSkills)
	}
	now := src.Now
	if now.IsZero() {
		now = time.Now()
	}
	g.Manifest = GalleryManifest{Kind: GalleryKind, Format: GalleryFormat, ID: c.ID, Name: c.DisplayName(), ThicketVersion: src.Version, CreatedAt: now.UTC().Truncate(time.Second)}
	entries, err := g.entries()
	if err != nil {
		return nil, err
	}
	var bad []string
	for name, data := range entries {
		if src.LooksSecret(string(data)) {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("not exported: something that looks like a secret is in %s; remove it and try again", strings.Join(bad, ", "))
	}
	// The same checks import makes, so an export is never refused there.
	total := 64 << 10
	for name, data := range entries {
		if len(data) > galleryMaxFileBytes {
			return nil, fmt.Errorf("not exported: %s is too large (at most %d KB per file)", name, galleryMaxFileBytes>>10)
		}
		if err := checkGalleryFile(name, data); err != nil {
			return nil, fmt.Errorf("not exported: %s: %w", name, err)
		}
		total += len(data)
	}
	if len(entries)+2 > galleryMaxFiles || total > galleryMaxTotal {
		return nil, fmt.Errorf("not exported: a gallery package holds at most %d files and %d MB", galleryMaxFiles, galleryMaxTotal>>20)
	}
	return g, nil
}

// entries are the package's files (without manifest and signature).
func (g *Gallery) entries() (map[string][]byte, error) {
	md, err := character.Marshal(g.Char)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{characterName: md}
	for rel, b := range g.Files {
		out[sprigDirPrefix+rel] = b
	}
	for _, k := range g.Skills {
		b, err := FormatSkillFile(k)
		if err != nil {
			return nil, err
		}
		out[skillsDirPrefix+k.ID+".md"] = b
	}
	for name := range out {
		if !galleryPath.MatchString(name) || name == manifestName || name == signatureName {
			return nil, fmt.Errorf("refusing to package %q", name)
		}
	}
	return out, nil
}

// Write signs the package with priv and writes it as a gzipped tar:
// manifest first, then the signature, then the files in order.
func (g *Gallery) Write(w io.Writer, priv ed25519.PrivateKey) error {
	entries, err := g.entries()
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	g.Signer = Signer{KeyID: KeyID(pub), PublicKey: pub}
	g.Manifest.AuthorKey = g.Signer.KeyID
	g.Manifest.Files = map[string]string{}
	names := make([]string, 0, len(entries))
	for name, b := range entries {
		g.Manifest.Files[name] = sum(b)
		names = append(names, name)
	}
	sort.Strings(names)
	man, err := json.MarshalIndent(g.Manifest, "", "  ")
	if err != nil {
		return err
	}
	// Never sign a manifest import would refuse (a long version, say).
	if _, err := readGalleryManifest(man); err != nil {
		return fmt.Errorf("not exported: %w", err)
	}
	if len(man) > galleryMaxFileBytes {
		return fmt.Errorf("not exported: the manifest is too large (at most %d KB)", galleryMaxFileBytes>>10)
	}
	sig, err := json.MarshalIndent(Signature{Alg: "ed25519", KeyID: g.Signer.KeyID, PublicKey: g.Signer.EncodedKey(),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, man))}, "", "  ")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: g.Manifest.CreatedAt, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := add(manifestName, man); err != nil {
		return err
	}
	if err := add(signatureName, sig); err != nil {
		return err
	}
	for _, name := range names {
		if err := add(name, entries[name]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Bytes is the signed package as a file.
func (g *Gallery) Bytes(priv ed25519.PrivateKey) ([]byte, error) {
	var b bytes.Buffer
	err := g.Write(&b, priv)
	return b.Bytes(), err
}

// IsGallery reports whether raw looks like a gallery package (it has a
// signature, or its manifest says so), so it's read by ReadGallery's
// strict rules rather than Read's. It reads headers and the manifest only.
func IsGallery(raw []byte) bool {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	tr := tar.NewReader(io.LimitReader(gz, maxTotalBytes+1<<20))
	for i := 0; i < maxFiles+2; i++ {
		h, err := tr.Next()
		if err != nil {
			return false
		}
		switch h.Name {
		case signatureName:
			return true
		case manifestName:
			var m struct {
				Kind string `json:"kind"`
			}
			b, _ := io.ReadAll(io.LimitReader(tr, galleryMaxFileBytes))
			if json.Unmarshal(b, &m) == nil && m.Kind == GalleryKind {
				return true
			}
		}
	}
	return false
}

// ReadGallery reads and checks a gallery package. Nothing is written. In
// order: every entry is a plain file at an allowed, clean path within the
// limits; the signature verifies over manifest.json and isn't by a key in
// revoked; the manifest passes its schema and lists exactly the files
// there, with matching SHA-256s; and every file is UTF-8, holds no MCP
// server or integration definition, and passes its schema and Thicket's
// own checks.
func ReadGallery(r io.Reader, revoked []string) (*Gallery, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, galleryMaxTotal+1<<20))
	if err != nil {
		return nil, errors.New("not a sprig package (it isn't a gzipped archive)")
	}
	stream := io.LimitReader(gz, galleryMaxTotal+1<<20)
	tr := tar.NewReader(stream)
	files := map[string][]byte{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Past the end marker there may be only zero padding, then a
			// clean end of the gzip stream (its checksum included):
			// nothing the signature doesn't cover rides along.
			rest, err := io.ReadAll(stream)
			if err != nil {
				return nil, fmt.Errorf("not a sprig package: %w", err)
			}
			if len(bytes.Trim(rest, "\x00")) > 0 {
				return nil, errors.New("the package has data after its last file")
			}
			if n, _ := gz.Read(make([]byte, 1)); n > 0 {
				return nil, fmt.Errorf("the package is too large (at most %d MB)", galleryMaxTotal>>20)
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a sprig package: %w", err)
		}
		if err := checkGalleryEntry(h); err != nil {
			return nil, err
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("the package holds %q twice", h.Name)
		}
		if len(files) >= galleryMaxFiles {
			return nil, fmt.Errorf("the package holds too many files (at most %d)", galleryMaxFiles)
		}
		if h.Size > galleryMaxFileBytes {
			return nil, fmt.Errorf("%s is too large (at most %d KB per file)", h.Name, galleryMaxFileBytes>>10)
		}
		b, err := io.ReadAll(io.LimitReader(tr, galleryMaxFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("not a sprig package: %w", err)
		}
		if len(b) > galleryMaxFileBytes {
			return nil, fmt.Errorf("%s is too large (at most %d KB per file)", h.Name, galleryMaxFileBytes>>10)
		}
		if total += len(b); total > galleryMaxTotal {
			return nil, fmt.Errorf("the package is too large (at most %d MB)", galleryMaxTotal>>20)
		}
		files[h.Name] = b
	}
	man, ok := files[manifestName]
	if !ok {
		return nil, errors.New("not a sprig package: it has no manifest.json")
	}
	sigb, ok := files[signatureName]
	if !ok {
		return nil, errors.New("this gallery package isn't signed (it has no signature file)")
	}
	// 1. The signature, before anything else is believed.
	signer, err := verifySignature(man, sigb)
	if err != nil {
		return nil, err
	}
	if slices.Contains(revoked, signer.KeyID) {
		return nil, fmt.Errorf("the author key %s has been revoked, so packages signed with it aren't imported", signer.KeyID)
	}
	// 2. The manifest it signed.
	m, err := readGalleryManifest(man)
	if err != nil {
		return nil, err
	}
	if m.AuthorKey != signer.KeyID {
		return nil, fmt.Errorf("the manifest names author key %s but it was signed by %s", m.AuthorKey, signer.KeyID)
	}
	// 3. Every file's hash, and nothing unlisted.
	delete(files, manifestName)
	delete(files, signatureName)
	for name := range m.Files {
		if name == manifestName || name == signatureName || !galleryPath.MatchString(name) {
			return nil, fmt.Errorf("the manifest lists %q, which a gallery package can't contain", clip(name))
		}
		if _, ok := files[name]; !ok {
			return nil, fmt.Errorf("the manifest lists %s, but it isn't in the package", name)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want, listed := m.Files[name]
		if !listed {
			return nil, fmt.Errorf("%s isn't listed in the signed manifest", name)
		}
		if want != sum(files[name]) {
			return nil, fmt.Errorf("%s doesn't match its checksum in the signed manifest: the package was changed after it was signed", name)
		}
	}
	// 4. Every file's content.
	for _, name := range names {
		if err := checkGalleryFile(name, files[name]); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	g := &Gallery{Manifest: *m, Signer: signer, Files: map[string][]byte{}}
	cb, ok := files[characterName]
	if !ok {
		return nil, errors.New("the package has no character.md")
	}
	if g.Char, err = character.Parse(cb); err != nil {
		return nil, fmt.Errorf("character.md: %w", err)
	}
	if g.Char.ID != m.ID {
		return nil, fmt.Errorf("the manifest is for %q but character.md is %q", m.ID, g.Char.ID)
	}
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, sprigDirPrefix):
			g.Files[strings.TrimPrefix(name, sprigDirPrefix)] = files[name]
		case strings.HasPrefix(name, skillsDirPrefix):
			k, _, err := ParseSkillFile(files[name])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			g.Skills = append(g.Skills, k)
		}
	}
	if len(g.Skills) > galleryMaxSkills {
		return nil, fmt.Errorf("the package has too many skills (at most %d)", galleryMaxSkills)
	}
	return g, nil
}

// checkGalleryEntry checks a tar header: a plain file at a clean, relative
// path on the allowlist, in a format the package may hold.
func checkGalleryEntry(h *tar.Header) error {
	name := h.Name
	switch {
	case h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink:
		return fmt.Errorf("the package holds %q, which is a link; a gallery package holds plain files only", clip(name))
	case h.Typeflag != tar.TypeReg:
		return fmt.Errorf("the package holds %q, which isn't a plain file", clip(name))
	case name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || (len(name) > 1 && name[1] == ':'):
		return fmt.Errorf("the package holds %q, an absolute path", clip(name))
	case slices.Contains(strings.Split(name, "/"), ".."):
		return fmt.Errorf("the package holds %q, which points outside the package (..)", clip(name))
	case path.Clean(name) != name:
		return fmt.Errorf("the package holds %q, which isn't a clean path", clip(name))
	case !utf8.ValidString(name):
		return errors.New("the package holds a file whose name isn't UTF-8")
	}
	if name != signatureName {
		switch strings.ToLower(path.Ext(name)) {
		case ".md", ".yaml", ".json":
		default:
			return fmt.Errorf("the package holds %q; a gallery package holds only Markdown, YAML and JSON files", clip(name))
		}
	}
	if !galleryPath.MatchString(name) {
		return fmt.Errorf("the package holds %q, which a gallery package can't contain (only character.md, sprig/avatar.json, sprig/voice_tests/*.yaml, skills/*.md, manifest.json and signature)", clip(name))
	}
	return nil
}

func verifySignature(man, sigb []byte) (Signer, error) {
	if err := checkText(sigb); err != nil {
		return Signer{}, fmt.Errorf("signature: %w", err)
	}
	var raw any
	if err := json.Unmarshal(sigb, &raw); err != nil {
		return Signer{}, fmt.Errorf("signature can't be read: %w", err)
	}
	if err := loadSchema("signature").validate(raw, "signature"); err != nil {
		return Signer{}, err
	}
	var s Signature
	if err := strictJSON(sigb, &s); err != nil {
		return Signer{}, fmt.Errorf("signature can't be read: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(s.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Signer{}, errors.New("signature: the public key isn't an Ed25519 key")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Signer{}, errors.New("signature: not an Ed25519 signature")
	}
	if KeyID(pub) != s.KeyID {
		return Signer{}, errors.New("signature: the key id doesn't belong to the public key with it")
	}
	if !ed25519.Verify(pub, man, sig) {
		return Signer{}, errors.New("the signature doesn't match the manifest: the package was changed after it was signed, or wasn't signed by the key it carries")
	}
	return Signer{KeyID: s.KeyID, PublicKey: pub}, nil
}

func readGalleryManifest(b []byte) (*GalleryManifest, error) {
	if err := checkText(b); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("the manifest can't be read: %w", err)
	}
	if obj, ok := raw.(map[string]any); ok {
		if f, ok := obj["format"].(float64); ok && f > GalleryFormat {
			return nil, fmt.Errorf("this gallery package is format %v; this Thicket reads format %d. Update Thicket to import it", f, GalleryFormat)
		}
	}
	if err := loadSchema("manifest").validate(raw, "manifest.json"); err != nil {
		return nil, err
	}
	var m GalleryManifest
	if err := strictJSON(b, &m); err != nil {
		return nil, fmt.Errorf("the manifest can't be read: %w", err)
	}
	return &m, nil
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// checkText refuses anything that isn't plain UTF-8 text.
func checkText(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("isn't UTF-8 text")
	}
	for _, r := range string(b) {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t' && r != '\r') || r == 0x7f {
			return errors.New("holds control characters, so it isn't plain text")
		}
	}
	return nil
}

// checkGalleryFile checks one file's content: text, no MCP or integration
// definitions, its schema, and Thicket's own checks for that kind of file.
func checkGalleryFile(name string, b []byte) error {
	if err := checkText(b); err != nil {
		return err
	}
	if m := mcpDefinition.Find(b); m != nil {
		return fmt.Errorf("it defines an MCP server or integration (%q); gallery sprigs can't bring their own", strings.TrimSpace(string(m)))
	}
	switch {
	case name == characterName:
		front, _, err := splitFront(b, true)
		if err != nil {
			return err
		}
		if err := validateYAML(front, "character", "front matter"); err != nil {
			return err
		}
		c, err := character.Parse(b)
		if err != nil {
			return err
		}
		if probs := c.Validate(); len(probs) > 0 {
			return errors.New(strings.Join(probs, "; "))
		}
		if c.IsKid() || c.Host {
			return errors.New("a gallery sprig can't be a kid sprig or the host")
		}
	case name == sprigDirPrefix+"avatar.json":
		var raw any
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		if err := loadSchema("avatar").validate(raw, "avatar.json"); err != nil {
			return err
		}
		var a avatar.Params
		if err := strictJSON(b, &a); err != nil {
			return err
		}
		return a.Validate()
	case strings.HasPrefix(name, sprigDirPrefix+"voice_tests/"):
		if err := validateYAML(b, "voicetest", "voice test"); err != nil {
			return err
		}
		var vt character.VoiceTest
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&vt); err != nil {
			return err
		}
		if strings.TrimSpace(vt.Prompt) == "" {
			return errors.New("prompt is required")
		}
	case strings.HasPrefix(name, skillsDirPrefix):
		k, _, err := ParseSkillFile(b)
		if err != nil {
			return err
		}
		if want := strings.TrimSuffix(strings.TrimPrefix(name, skillsDirPrefix), ".md"); k.ID != want {
			return fmt.Errorf("id %q must match the file name (%s.md)", k.ID, k.ID)
		}
	default:
		return errors.New("not a file a gallery package can contain")
	}
	return nil
}

// validateYAML decodes YAML and checks it against a schema. It must be
// one document: a second one would be installed unchecked.
func validateYAML(b []byte, schemaName, what string) error {
	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", what, err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: must be a single YAML document", what)
	}
	raw, err := normalize(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return loadSchema(schemaName).validate(raw, what)
}

// splitFront splits "---\nfront\n---\nbody" the way character.Parse does
// (strict: the character file), or as skill files do.
func splitFront(b []byte, char bool) (front, body []byte, err error) {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return nil, nil, errors.New("file must start with a '---' front matter block")
	}
	sep := []byte("\n---\n")
	if char {
		sep = []byte("\n---")
	}
	i := bytes.Index(rest, sep)
	if i < 0 {
		return nil, nil, errors.New("front matter is not closed with '---'")
	}
	return rest[:i], rest[i+len(sep):], nil
}

// skillFront is a skill file's front matter, as agent.FormatSkill writes
// it (the two must stay the same).
type skillFront struct {
	ID           string             `yaml:"id"`
	Name         string             `yaml:"name"`
	Description  string             `yaml:"description"`
	Inputs       []store.SkillInput `yaml:"inputs,omitempty"`
	SuccessCheck string             `yaml:"success_check,omitempty"`
}

// FormatSkillFile writes a skill as Markdown with YAML front matter, in
// the format of a sprig's skills folder.
func FormatSkillFile(k store.Skill) ([]byte, error) {
	front, err := yaml.Marshal(skillFront{k.ID, k.Name, k.Description, k.Inputs, k.SuccessCheck})
	if err != nil {
		return nil, err
	}
	return []byte("---\n" + string(front) + "---\n" + strings.TrimSpace(k.Body) + "\n"), nil
}

// ParseSkillFile reads a gallery skill file against the skill schema. It
// returns the skill and its body.
func ParseSkillFile(b []byte) (store.Skill, string, error) {
	front, body, err := splitFront(b, false)
	if err != nil {
		return store.Skill{}, "", err
	}
	if err := validateYAML(front, "skill", "front matter"); err != nil {
		return store.Skill{}, "", err
	}
	var f skillFront
	dec := yaml.NewDecoder(bytes.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return store.Skill{}, "", fmt.Errorf("front matter: %w", err)
	}
	text := strings.TrimSpace(string(body))
	if text == "" || utf8.RuneCountInString(text) > galleryMaxSkillBody {
		return store.Skill{}, "", fmt.Errorf("steps are required (max %d characters)", galleryMaxSkillBody)
	}
	if strings.Contains(text, "<untrusted") {
		return store.Skill{}, "", errors.New("steps can't contain <untrusted> content")
	}
	return store.Skill{ID: f.ID, Name: strings.TrimSpace(f.Name), Description: strings.TrimSpace(f.Description), Body: text,
		Inputs: f.Inputs, SuccessCheck: strings.TrimSpace(f.SuccessCheck)}, text, nil
}

// --- import ---

// RevokedKeys reads the revoked author key ids: the setting
// gallery.revoked_keys (any separators), until the gallery index brings
// its own list.
func RevokedKeys(ctx context.Context, db *store.Store) ([]string, error) {
	v, _, err := db.Setting(ctx, store.GalleryRevokedKey)
	if err != nil {
		return nil, err
	}
	return ParseRevoked(v), nil
}

// ParseRevoked splits a revocation list into key ids.
func ParseRevoked(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// Trust is what an import knows about the package's author key.
type Trust struct {
	// Known: this key was trusted before (first seen with FirstSprig).
	Known      bool
	FirstSeen  time.Time
	FirstSprig string
	// Update: the sprig is already here, from the gallery, signed by this
	// same key; the import updates it.
	Update bool
}

// CheckTrust applies trust on first use. A revoked key is refused; a key
// id stored with another public key is refused; a sprig already here is
// updated only if it came from the gallery with this same key, and is
// otherwise a clash.
func (g *Gallery) CheckTrust(ctx context.Context, db *store.Store, charsDir string, revoked []string) (Trust, error) {
	var t Trust
	if slices.Contains(revoked, g.Signer.KeyID) {
		return t, fmt.Errorf("the author key %s has been revoked, so packages signed with it aren't imported", g.Signer.KeyID)
	}
	k, err := db.GalleryKey(ctx, g.Signer.KeyID)
	switch {
	case err == nil:
		if k.PublicKey != g.Signer.EncodedKey() {
			return t, fmt.Errorf("a different author key is already known under the id %s; this package is refused", g.Signer.KeyID)
		}
		t.Known, t.FirstSeen, t.FirstSprig = true, k.FirstSeen, k.FirstSprig
	case !errors.Is(err, store.ErrNotFound):
		return t, err
	}
	id := g.Char.ID
	if _, err := os.Stat(filepath.Join(charsDir, id+".md")); err != nil {
		return t, nil // a new sprig
	}
	prev, err := db.GallerySprig(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return t, nil // here, but not from the gallery: CheckClash says so
	}
	if err != nil {
		return t, err
	}
	if prev.KeyID != g.Signer.KeyID {
		return t, fmt.Errorf("this is an update of %s, which came signed by author key %s; this package is signed by %s, a different key, so it's refused", id, prev.KeyID, g.Signer.KeyID)
	}
	t.Update = true
	return t, nil
}

// Prepare applies the import rules no package can override: an optional
// new id and name (not for an update), no scopes granted (the ones asked
// for go in Asks), never the host, never a kid sprig.
func (g *Gallery) Prepare(id, name string, update bool) error {
	c := g.Char
	if c.IsKid() || c.Household == character.TierKid {
		return errors.New("a sprig can't become a kid sprig through a gallery import")
	}
	if !update {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			c.ID = id
		}
		if name = strings.TrimSpace(name); name != "" {
			c.Name, c.Nickname = name, ""
		}
	}
	g.Asks = append([]string(nil), c.Tools...)
	c.Tools = nil
	c.Host, c.BelongsTo, c.Kids, c.Parents, c.Allow, c.Household = false, 0, nil, nil, nil, ""
	c.Limits, c.Avatar, c.Model = character.KidLimits{}, "", ""
	c.CreatedBy = "gallery"
	if probs := c.Validate(); len(probs) > 0 {
		return errors.New(strings.Join(probs, "; "))
	}
	return nil
}

// PlanSkills drops package skills whose id is already used here by the
// shared library or another sprig (they're listed in Skipped), and checks
// the rest with valid (agent.ValidateSkill).
func (g *Gallery) PlanSkills(ctx context.Context, db *store.Store, valid func(store.Skill) error) error {
	var keep []store.Skill
	g.Skipped, g.SkippedSkills = nil, nil
	for _, k := range g.Skills {
		if valid != nil {
			if err := valid(k); err != nil {
				return fmt.Errorf("the skill %q in this package isn't valid: %w", k.ID, err)
			}
		}
		cur, err := db.SkillByID(ctx, k.ID, 0)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case cur.Shared():
			g.Skipped = append(g.Skipped, fmt.Sprintf("%s (your shared library already has a skill with this id; the sprig is offered that one)", k.ID))
			g.SkippedSkills = append(g.SkippedSkills, k)
			continue
		case cur.SprigID != g.Char.ID:
			g.Skipped = append(g.Skipped, fmt.Sprintf("%s (another sprig here has its own skill with this id)", k.ID))
			g.SkippedSkills = append(g.SkippedSkills, k)
			continue
		}
		keep = append(keep, k)
	}
	g.Skills = keep
	return nil
}

// Preview is what a person reads before a gallery import: the author key
// and whether it's known, every file in full, every scope the sprig asks
// for, and the rules it arrives under.
func (g *Gallery) Preview(t Trust) (string, error) {
	c := g.Char
	md, err := character.Marshal(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Signed by author key %s\nFingerprint: %s\n", g.Signer.KeyID, g.Signer.Fingerprint())
	switch {
	case t.Known:
		fmt.Fprintf(&b, "Known author: trusted since %s (first with %s).\n", t.FirstSeen.Format("2 Jan 2006"), t.FirstSprig)
	default:
		b.WriteString("New author: this key hasn't been seen here. Importing trusts it from now on, and updates of this sprig must be signed by it. Compare the fingerprint with the one the author publishes.\n")
	}
	if t.Update {
		fmt.Fprintf(&b, "This updates %s, which came from the same author. Its scopes, levels and who it belongs to stay as they are.\n", c.ID)
	}
	b.WriteString("```diff\n")
	b.WriteString(NewFileDiff(c.ID+".md", string(md)))
	rels := make([]string, 0, len(g.Files))
	for rel := range g.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		b.WriteString(NewFileDiff(c.ID+"/"+rel, string(g.Files[rel])))
	}
	for _, k := range g.Skills {
		f, err := FormatSkillFile(k)
		if err != nil {
			return "", err
		}
		b.WriteString(NewFileDiff(c.ID+"/skills/"+k.ID+".md"+PendingSuffix, string(f)))
	}
	for _, k := range g.SkippedSkills {
		f, err := FormatSkillFile(k)
		if err != nil {
			return "", err
		}
		b.WriteString(NewFileDiff("(in the package, NOT imported) skills/"+k.ID+".md", string(f)))
	}
	b.WriteString("```\n")
	if len(g.Skills) > 0 {
		b.WriteString("Skills arrive as the sprig's own, inactive until you approve each one.\n")
	}
	for _, s := range g.Skipped {
		fmt.Fprintf(&b, "Skill not imported: %s.\n", s)
	}
	if len(g.Asks) > 0 {
		fmt.Fprintf(&b, "It asks for these scopes, none of which are granted (grant any you want in chat): %s\n", strings.Join(g.Asks, ", "))
	} else {
		b.WriteString("It asks for no scopes.\n")
	}
	if !t.Update {
		b.WriteString("Every tool starts at draft. ")
	}
	b.WriteString("No routines, memories, chats or secrets come with it.\n")
	return b.String(), nil
}

// GalleryInstallOptions say who imports and how.
type GalleryInstallOptions struct {
	// By is the person importing: the key is trusted by them.
	By int64
	// Update replaces an earlier import of this sprig (CheckTrust said so).
	Update bool
}

// GalleryInstalled is what an install did.
type GalleryInstalled struct {
	Path    string
	Pending []string // skill ids waiting for approval
}

// Install writes a prepared gallery package. A new sprig gets its folder
// files, its skills as pending, every tool at draft, then its character
// file; anything that fails before the character file is undone. An
// update keeps the local sprig's scopes, owner, tier and levels and
// replaces what the package describes. The author key is trusted and the
// sprig recorded as coming from it.
func (g *Gallery) Install(ctx context.Context, db *store.Store, charsDir string, opts GalleryInstallOptions) (*GalleryInstalled, error) {
	// Checked again here, after the person said yes: a key revoked while
	// the preview waited isn't installed (CLI and chat both come here).
	revoked, err := RevokedKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	if slices.Contains(revoked, g.Signer.KeyID) {
		return nil, fmt.Errorf("the author key %s has been revoked, so packages signed with it aren't imported", g.Signer.KeyID)
	}
	if err := os.MkdirAll(charsDir, 0o755); err != nil {
		return nil, err
	}
	c := g.Char
	unlock, err := lockSprig(charsDir, c.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if opts.Update {
		return g.update(ctx, db, charsDir, opts)
	}
	if _, err := os.Stat(filepath.Join(charsDir, c.ID+".md")); err == nil {
		return nil, &Clash{What: "id", Value: c.ID}
	}
	bg := context.WithoutCancel(ctx)
	var written []string
	var oldLevels []store.ToolLevelRow
	levelSet, recorded := false, false
	undo := func(cause error) error {
		var left []string
		if recorded {
			if err := db.DeleteGallerySprig(bg, c.ID); err != nil {
				left = append(left, "its gallery record")
			}
		}
		if levelSet {
			if err := db.SetToolLevel(bg, c.ID, store.AnyTool, ""); err != nil {
				left = append(left, "its draft level")
			}
		}
		for _, r := range oldLevels {
			if err := db.SetToolLevel(bg, r.Character, r.Tool, r.Level); err != nil {
				left = append(left, "the level for "+r.Tool)
			}
		}
		for i := len(written) - 1; i >= 0; i-- {
			if err := os.Remove(written[i]); err != nil && !os.IsNotExist(err) {
				left = append(left, written[i])
			}
		}
		for _, d := range []string{"voice_tests", "skills", ""} {
			os.Remove(filepath.Join(charsDir, c.ID, d))
		}
		if len(left) > 0 {
			return fmt.Errorf("%w; undoing it failed too, so remove these by hand: %s", cause, strings.Join(left, ", "))
		}
		return fmt.Errorf("%w (nothing was imported)", cause)
	}
	put := func(full string, b []byte) error {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s already exists; not overwriting it", full)
			}
			return err
		}
		written = append(written, full)
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	for _, rel := range sortedKeys(g.Files) {
		if !galleryPath.MatchString(sprigDirPrefix + rel) {
			return nil, undo(fmt.Errorf("refusing to write %q", rel))
		}
		if err := put(filepath.Join(charsDir, c.ID, filepath.FromSlash(rel)), g.Files[rel]); err != nil {
			return nil, undo(err)
		}
	}
	res := &GalleryInstalled{}
	for _, k := range g.Skills {
		b, err := FormatSkillFile(k)
		if err != nil {
			return nil, undo(err)
		}
		if err := put(pendingPath(charsDir, c.ID, k.ID), b); err != nil {
			return nil, undo(err)
		}
		res.Pending = append(res.Pending, k.ID)
	}
	all, err := db.ToolLevels(ctx)
	if err != nil {
		return nil, undo(err)
	}
	for _, r := range all {
		if r.Character != c.ID || r.Tool == store.AnyTool {
			continue
		}
		if err := db.SetToolLevel(ctx, c.ID, r.Tool, ""); err != nil {
			return nil, undo(err)
		}
		oldLevels = append(oldLevels, r)
	}
	if prev, err := db.ExactToolLevel(ctx, c.ID, store.AnyTool); err == nil && prev != "" {
		oldLevels = append(oldLevels, store.ToolLevelRow{Character: c.ID, Tool: store.AnyTool, Level: prev})
	}
	if err := db.SetToolLevel(ctx, c.ID, store.AnyTool, store.LevelDraft); err != nil {
		return nil, undo(err)
	}
	levelSet = true
	if err := db.PutGallerySprig(ctx, store.GallerySprig{SprigID: c.ID, PackageID: g.Manifest.ID, KeyID: g.Signer.KeyID}); err != nil {
		return nil, undo(err)
	}
	recorded = true
	md, err := character.Marshal(c)
	if err != nil {
		return nil, undo(err)
	}
	dest := filepath.Join(charsDir, c.ID+".md")
	tmp, err := os.CreateTemp(charsDir, "."+c.ID+"-*.tmp")
	if err != nil {
		return nil, undo(err)
	}
	_, err = tmp.Write(md)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Link(tmp.Name(), dest)
	}
	os.Remove(tmp.Name())
	if err != nil {
		return nil, undo(err)
	}
	res.Path = dest
	// Trusted once the sprig is in: a failed import trusts nothing. If
	// this write fails, the sprig's gallery record is already there, so
	// importing the package again takes the update path, which stores the
	// key (update does).
	if err := db.TrustGalleryKey(bg, store.GalleryKey{KeyID: g.Signer.KeyID, PublicKey: g.Signer.EncodedKey(), FirstSprig: c.ID, TrustedBy: opts.By}); err != nil {
		return res, fmt.Errorf("%s was imported, but its author key couldn't be stored (import the same package again to finish): %w", c.DisplayName(), err)
	}
	return res, nil
}

// lockSprig reserves a sprig id while its files are written (an import,
// an update, or a pending skill's approval), across processes: the CLI and
// a running Thicket use the same lock file.
func lockSprig(charsDir, id string) (func(), error) {
	lock := filepath.Join(charsDir, id+lockSuffix)
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%q is being imported or changed already; try again in a moment", id)
		}
		return nil, err
	}
	lf.Close()
	return func() { os.Remove(lock) }, nil
}

// update replaces an earlier import with a newer package from the same
// author. What belongs to this install (scopes, owner, tier, levels,
// picture, model) stays; the description, folder files and skills come
// from the package (skills pending approval again).
func (g *Gallery) update(ctx context.Context, db *store.Store, charsDir string, opts GalleryInstallOptions) (*GalleryInstalled, error) {
	id := g.Char.ID
	path := filepath.Join(charsDir, id+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	local, err := character.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("the sprig here can't be read, so it isn't updated: %w", err)
	}
	if local.IsKid() {
		return nil, errors.New("kid sprigs can't have gallery imports")
	}
	prev, err := db.GallerySprig(ctx, id)
	if err != nil || prev.KeyID != g.Signer.KeyID {
		return nil, errors.New("this sprig didn't come from this author, so it isn't updated")
	}
	c := g.Char
	c.Tools, c.BelongsTo, c.Household, c.Host = local.Tools, local.BelongsTo, local.Household, local.Host
	c.Avatar, c.Model, c.CreatedBy = local.Avatar, local.Model, local.CreatedBy
	if probs := c.Validate(); len(probs) > 0 {
		return nil, errors.New(strings.Join(probs, "; "))
	}
	res := &GalleryInstalled{Path: path}
	// Every new file is written beside its destination first; only when
	// all are staged are they moved into place, and if one move fails the
	// ones already moved are put back as they were.
	var plan []stagedFile
	for _, rel := range sortedKeys(g.Files) {
		if !galleryPath.MatchString(sprigDirPrefix + rel) {
			return nil, fmt.Errorf("refusing to write %q", rel)
		}
		plan = append(plan, stagedFile{dest: filepath.Join(charsDir, id, filepath.FromSlash(rel)), data: g.Files[rel]})
	}
	for _, k := range g.Skills {
		f, err := FormatSkillFile(k)
		if err != nil {
			return nil, err
		}
		plan = append(plan, stagedFile{dest: pendingPath(charsDir, id, k.ID), data: f})
		res.Pending = append(res.Pending, k.ID)
	}
	md, err := character.Marshal(c)
	if err != nil {
		return nil, err
	}
	plan = append(plan, stagedFile{dest: path, data: md}) // last: the sprig changes once the rest is in
	if err := publishAll(plan); err != nil {
		return nil, fmt.Errorf("%w (the sprig was left as it was)", err)
	}
	bg := context.WithoutCancel(ctx)
	if err := db.PutGallerySprig(bg, store.GallerySprig{SprigID: id, PackageID: g.Manifest.ID, KeyID: g.Signer.KeyID}); err != nil {
		return res, err
	}
	// Finishes a first import whose key write failed (Install).
	if err := db.TrustGalleryKey(bg, store.GalleryKey{KeyID: g.Signer.KeyID, PublicKey: g.Signer.EncodedKey(), FirstSprig: id, TrustedBy: opts.By}); err != nil {
		return res, fmt.Errorf("%s was updated, but its author key couldn't be stored: %w", c.DisplayName(), err)
	}
	return res, nil
}

// stagedFile is one file an update writes.
type stagedFile struct {
	dest string
	data []byte
	tmp  string
	old  []byte // what was there (when existed)
	had  bool
}

// publishAll writes every file in plan, all or none: each is staged in a
// temporary file next to its destination, then the temporary files are
// renamed into place in order. If a rename fails, the files already
// replaced get their old contents back (and new ones are removed).
func publishAll(plan []stagedFile) error {
	cleanup := func() {
		for _, f := range plan {
			if f.tmp != "" {
				os.Remove(f.tmp)
			}
		}
	}
	for i := range plan {
		f := &plan[i]
		if old, err := os.ReadFile(f.dest); err == nil {
			f.old, f.had = old, true
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return err
		}
		if err := os.MkdirAll(filepath.Dir(f.dest), 0o755); err != nil {
			cleanup()
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(f.dest), ".gallery-*")
		if err != nil {
			cleanup()
			return err
		}
		f.tmp = tmp.Name()
		_, err = tmp.Write(f.data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.tmp, 0o644)
		}
		if err != nil {
			cleanup()
			return err
		}
	}
	for i := range plan {
		if err := os.Rename(plan[i].tmp, plan[i].dest); err != nil {
			cleanup()
			var left []string
			for j := i - 1; j >= 0; j-- {
				var rerr error
				if plan[j].had {
					rerr = writeAtomic(plan[j].dest, plan[j].old)
				} else {
					rerr = os.Remove(plan[j].dest)
				}
				if rerr != nil {
					left = append(left, plan[j].dest)
				}
			}
			if len(left) > 0 {
				return fmt.Errorf("%w; putting back the files already changed failed too: check %s", err, strings.Join(left, ", "))
			}
			return err
		}
		plan[i].tmp = ""
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeAtomic(full string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".gallery-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), full)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// --- pending skills ---

func pendingPath(charsDir, sprig, id string) string {
	return filepath.Join(character.SkillDir(charsDir, sprig), id+".md"+PendingSuffix)
}

var pendingID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// PendingSkills lists a sprig's skills waiting for approval.
func PendingSkills(charsDir, sprig string) ([]store.Skill, error) {
	entries, err := os.ReadDir(character.SkillDir(charsDir, sprig))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []store.Skill
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".md"+PendingSuffix)
		if !ok || e.IsDir() || !pendingID.MatchString(id) {
			continue
		}
		k, err := PendingSkill(charsDir, sprig, id)
		if err != nil {
			continue
		}
		out = append(out, *k)
	}
	return out, nil
}

// PendingSkill reads one pending skill.
func PendingSkill(charsDir, sprig, id string) (*store.Skill, error) {
	if !pendingID.MatchString(id) {
		return nil, fmt.Errorf("%s has no skill %q waiting for approval", sprig, id)
	}
	b, err := os.ReadFile(pendingPath(charsDir, sprig, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s has no skill %q waiting for approval", sprig, id)
	}
	if err != nil {
		return nil, err
	}
	k, _, err := ParseSkillFile(b)
	if err != nil {
		return nil, err
	}
	if k.ID != id {
		return nil, fmt.Errorf("the pending skill file %s.md%s says it's %q", id, PendingSuffix, k.ID)
	}
	k.SprigID = sprig
	return &k, nil
}

// ErrPendingChanged means a pending skill isn't the version that was
// shown, so it wasn't approved.
var ErrPendingChanged = errors.New("the skill changed since it was shown, so it wasn't approved; ask again to see it as it is now")

// ApprovePendingSkill makes a pending skill the sprig's own: the file is
// renamed to <id>.md, which the skill loader picks up (a newer version of
// a skill it already has replaces it, with history kept). want, if not
// nil, is the skill file as the person was shown it (FormatSkillFile): a
// different version (a gallery update since) isn't approved. The check
// and the rename hold the sprig's lock, which gallery updates take too.
func ApprovePendingSkill(charsDir, sprig, id string, want []byte) (*store.Skill, error) {
	unlock, err := lockSprig(charsDir, sprig)
	if err != nil {
		return nil, err
	}
	defer unlock()
	k, err := PendingSkill(charsDir, sprig, id)
	if err != nil {
		return nil, err
	}
	if want != nil {
		if f, err := FormatSkillFile(*k); err != nil || !bytes.Equal(f, want) {
			return nil, ErrPendingChanged
		}
	}
	p := pendingPath(charsDir, sprig, id)
	if err := os.Rename(p, strings.TrimSuffix(p, PendingSuffix)); err != nil {
		return nil, err
	}
	return k, nil
}

// RestorePendingSkill puts an approved skill back to waiting (its
// approval couldn't be completed).
func RestorePendingSkill(charsDir, sprig, id string) error {
	p := pendingPath(charsDir, sprig, id)
	return os.Rename(strings.TrimSuffix(p, PendingSuffix), p)
}
