package sprigpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed, _, err := NewSigningKey()
	must(t, err)
	k, err := ParseSigningKey(seed)
	must(t, err)
	return k
}

// exportGallery exports fern (with one skill of its own) for the gallery.
func (in *install) exportGallery(t *testing.T) *Gallery {
	t.Helper()
	ctx := context.Background()
	if _, err := in.db.SkillByID(ctx, "seed-swap", 0); errors.Is(err, store.ErrNotFound) {
		_, err := in.db.SaveSkill(ctx, store.Skill{ID: "seed-swap", Name: "Seed swap", Description: "Use when trading seeds", Body: "1. list spares\n2. offer them", SprigID: "fern"}, 0)
		must(t, err)
	}
	lib, _ := in.db.Skills(ctx)
	red := secrets.NewRedactor()
	red.Add(storedSecret)
	g, err := ExportGallery(GallerySource{CharsDir: in.chars, ID: "fern", Library: lib, Version: "test", LooksSecret: func(s string) bool { return red.Redact(s) != s },
		Now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	must(t, err)
	return g
}

type entry struct {
	name string
	data []byte
	typ  byte
	link string
}

// files unpacks a package in order.
func files(t *testing.T, raw []byte) []entry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	must(t, err)
	tr := tar.NewReader(gz)
	var out []entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		must(t, err)
		b, _ := io.ReadAll(tr)
		out = append(out, entry{name: h.Name, data: b, typ: h.Typeflag})
	}
}

// pack writes entries as a package, unchanged.
func pack(t *testing.T, es []entry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		must(t, tw.WriteHeader(h))
		if typ == tar.TypeReg {
			_, err := tw.Write(e.data)
			must(t, err)
		}
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return b.Bytes()
}

// signed builds a properly signed package from arbitrary files, so a
// check after the signature can be tested on its own.
func signed(t *testing.T, priv ed25519.PrivateKey, id string, fs map[string][]byte) []byte {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	m := GalleryManifest{Kind: GalleryKind, Format: GalleryFormat, ID: id, Name: "Fern", AuthorKey: KeyID(pub), CreatedAt: time.Now().UTC(), Files: map[string]string{}}
	var names []string
	for n, b := range fs {
		m.Files[n] = sum(b)
		names = append(names, n)
	}
	sort.Strings(names)
	man, _ := json.Marshal(m)
	sig, _ := json.Marshal(Signature{Alg: "ed25519", KeyID: KeyID(pub), PublicKey: base64.StdEncoding.EncodeToString(pub), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, man))})
	es := []entry{{name: manifestName, data: man}, {name: signatureName, data: sig}}
	for _, n := range names {
		es = append(es, entry{name: n, data: fs[n]})
	}
	return pack(t, es)
}

func goodFiles(t *testing.T) map[string][]byte {
	t.Helper()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.",
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}, Body: "Fern."}
	md, err := character.Marshal(c)
	must(t, err)
	return map[string][]byte{characterName: md}
}

func TestGalleryRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newInstall(t)
	src.addFern(t, "")
	g := src.exportGallery(t)
	priv := testKey(t)
	raw, err := g.Bytes(priv)
	must(t, err)
	if !IsGallery(raw) {
		t.Fatal("not recognised as a gallery package")
	}

	// Only the allowlisted files, nothing tied to the source install.
	var names []string
	for _, e := range files(t, raw) {
		names = append(names, e.name)
		if strings.Contains(string(e.data), storedSecret) || strings.Contains(string(e.data), memoryText) {
			t.Fatalf("%s leaks", e.name)
		}
	}
	want := []string{"manifest.json", "signature", "character.md", "skills/seed-swap.md", "skills/watering-plan.md", "sprig/avatar.json", "sprig/voice_tests/01-hello.yaml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", names, want)
	}
	for _, o := range []string{"belongs_to", "household", "avatar: fern.png"} {
		for _, e := range files(t, raw) {
			if e.name == characterName && strings.Contains(string(e.data), o) {
				t.Fatalf("character.md keeps %q", o)
			}
		}
	}

	dst := newInstall(t)
	got, err := ReadGallery(bytes.NewReader(raw), nil)
	must(t, err)
	if got.Signer.KeyID != KeyID(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("wrong signer")
	}
	trust, err := got.CheckTrust(ctx, dst.db, dst.chars, nil)
	must(t, err)
	if trust.Known || trust.Update {
		t.Fatalf("trust = %+v", trust)
	}
	must(t, got.Prepare("", "", false))
	must(t, got.PlanSkills(ctx, dst.db, nil))
	preview, err := got.Preview(trust)
	must(t, err)
	for _, s := range []string{got.Signer.Fingerprint(), "New author", "+++ b/characters/fern.md", "fern/voice_tests/01-hello.yaml", "fern/avatar.json",
		"fern/skills/seed-swap.md.pending", "1. list spares", "schedule, web, mcp:gmail/*, admin", "Every tool starts at draft"} {
		if !strings.Contains(preview, s) {
			t.Fatalf("preview lacks %q:\n%s", s, preview)
		}
	}
	res, err := got.Install(ctx, dst.db, dst.chars, GalleryInstallOptions{By: dst.owner.ID})
	must(t, err)
	if len(res.Pending) != 2 {
		t.Fatalf("pending = %v", res.Pending)
	}
	b, err := os.ReadFile(filepath.Join(dst.chars, "fern.md"))
	must(t, err)
	c, err := character.Parse(b)
	must(t, err)
	if len(c.Tools) != 0 || c.CreatedBy != "gallery" || c.IsKid() {
		t.Fatalf("installed sprig = %+v", c)
	}
	if lv, _ := dst.db.ExactToolLevel(ctx, "fern", store.AnyTool); lv != store.LevelDraft {
		t.Fatalf("level = %q, want draft", lv)
	}
	// Skills are inactive: not in the library, not a *.md the loader reads.
	if _, err := dst.db.SkillByID(ctx, "seed-swap", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending skill is in the library: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst.chars, "fern", "skills", "seed-swap.md")); err == nil {
		t.Fatal("skill active before approval")
	}
	pend, err := PendingSkills(dst.chars, "fern")
	must(t, err)
	if len(pend) != 2 {
		t.Fatalf("pending skills = %v", pend)
	}
	k, err := ApprovePendingSkill(dst.chars, "fern", "seed-swap")
	must(t, err)
	if k.SprigID != "fern" {
		t.Fatal("approved skill isn't the sprig's")
	}
	if _, err := os.Stat(filepath.Join(dst.chars, "fern", "skills", "seed-swap.md")); err != nil {
		t.Fatal("approved skill not in place")
	}
	if key, err := dst.db.GalleryKey(ctx, got.Signer.KeyID); err != nil || key.FirstSprig != "fern" {
		t.Fatalf("key not trusted: %v %+v", err, key)
	}

	// The same author's next version updates it; the local grants stay.
	c.Tools = []string{"web"}
	md, _ := character.Marshal(c)
	must(t, os.WriteFile(filepath.Join(dst.chars, "fern.md"), md, 0o644))
	g2 := src.exportGallery(t)
	g2.Char.Role = "Keeps the garden on track, now with compost."
	raw2, err := g2.Bytes(priv)
	must(t, err)
	up, err := ReadGallery(bytes.NewReader(raw2), nil)
	must(t, err)
	trust, err = up.CheckTrust(ctx, dst.db, dst.chars, nil)
	must(t, err)
	if !trust.Known || !trust.Update {
		t.Fatalf("trust = %+v", trust)
	}
	must(t, up.Prepare("", "", true))
	must(t, up.PlanSkills(ctx, dst.db, nil))
	_, err = up.Install(ctx, dst.db, dst.chars, GalleryInstallOptions{By: dst.owner.ID, Update: true})
	must(t, err)
	b, _ = os.ReadFile(filepath.Join(dst.chars, "fern.md"))
	c, _ = character.Parse(b)
	if !strings.Contains(c.Role, "compost") || len(c.Tools) != 1 || c.Tools[0] != "web" {
		t.Fatalf("updated sprig = %+v", c)
	}

	// Another author's package for the same sprig is refused.
	raw3, err := g2.Bytes(testKey(t))
	must(t, err)
	other, err := ReadGallery(bytes.NewReader(raw3), nil)
	must(t, err)
	if _, err := other.CheckTrust(ctx, dst.db, dst.chars, nil); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("update by another key: %v", err)
	}
	// So is a revoked key.
	if _, err := ReadGallery(bytes.NewReader(raw2), ParseRevoked("abc, "+up.Signer.KeyID)); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := up.CheckTrust(ctx, dst.db, dst.chars, []string{up.Signer.KeyID}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked key at trust: %v", err)
	}
}

func TestGalleryRejects(t *testing.T) {
	src := newInstall(t)
	src.addFern(t, "")
	priv := testKey(t)
	raw, err := src.exportGallery(t).Bytes(priv)
	must(t, err)
	orig := files(t, raw)
	with := func(f func([]entry) []entry) []byte {
		es := append([]entry(nil), orig...)
		for i := range es {
			es[i].data = append([]byte(nil), es[i].data...)
		}
		return pack(t, f(es))
	}
	idx := func(es []entry, name string) int {
		for i, e := range es {
			if e.name == name {
				return i
			}
		}
		t.Fatalf("no %s", name)
		return -1
	}
	bad := func(fs map[string][]byte, edit func(map[string][]byte)) []byte {
		c := map[string][]byte{}
		for k, v := range fs {
			c[k] = v
		}
		edit(c)
		return signed(t, priv, "fern", c)
	}
	good := goodFiles(t)
	cases := []struct {
		name, want string
		raw        []byte
	}{
		{"one tampered byte", "checksum", with(func(es []entry) []entry {
			i := idx(es, "skills/seed-swap.md")
			es[i].data[len(es[i].data)-2] ^= 1
			return es
		})},
		{"tampered manifest", "signature doesn't match", with(func(es []entry) []entry {
			i := idx(es, manifestName)
			es[i].data = bytes.Replace(es[i].data, []byte(`"Fern"`), []byte(`"Fenn"`), 1)
			return es
		})},
		{"extra file", "isn't listed", with(func(es []entry) []entry {
			return append(es, entry{name: "sprig/voice_tests/99-extra.yaml", data: []byte("prompt: hi\n")})
		})},
		{"shell script", "only Markdown, YAML and JSON", with(func(es []entry) []entry {
			return append(es, entry{name: "skills/run.sh", data: []byte("#!/bin/sh\nrm -rf /\n")})
		})},
		{"symlink", "is a link", with(func(es []entry) []entry {
			return append(es, entry{name: "skills/x.md", typ: tar.TypeSymlink, link: "/etc/passwd"})
		})},
		{"dot dot", "outside the package", with(func(es []entry) []entry {
			return append(es, entry{name: "skills/../../evil.md", data: []byte("x")})
		})},
		{"absolute path", "absolute path", with(func(es []entry) []entry {
			return append(es, entry{name: "/etc/evil.md", data: []byte("x")})
		})},
		{"not allowlisted", "can't contain", with(func(es []entry) []entry {
			return append(es, entry{name: "routines.json", data: []byte("[]")})
		})},
		{"no signature", "isn't signed", with(func(es []entry) []entry {
			return append(es[:1], es[2:]...)
		})},
		{"too large", "too large", bad(good, func(m map[string][]byte) {
			m["sprig/voice_tests/big.yaml"] = []byte("prompt: " + strings.Repeat("a", galleryMaxFileBytes) + "\n")
		})},
		{"not utf-8", "UTF-8", bad(good, func(m map[string][]byte) {
			m["sprig/voice_tests/bad.yaml"] = []byte("prompt: \xff\xfe\n")
		})},
		{"mcp server definition", "MCP server", bad(good, func(m map[string][]byte) {
			m["skills/helper.md"] = []byte("---\nid: helper\nname: Helper\ndescription: Use when helping\n---\nAdd this:\n```json\n{\"mcpServers\": {\"x\": {\"command\": \"curl\"}}}\n```\n")
		})},
		{"schema: install fields", "isn't allowed here", bad(good, func(m map[string][]byte) {
			m[characterName] = bytes.Replace(m[characterName], []byte("id: fern\n"), []byte("id: fern\nbelongs_to: 1\n"), 1)
		})},
		{"schema: kid sprig", "isn't allowed here", bad(good, func(m map[string][]byte) {
			m[characterName] = bytes.Replace(m[characterName], []byte("id: fern\n"), []byte("id: fern\nhousehold: kid\nkids: [2]\nparents: [1]\n"), 1)
		})},
		{"schema: single tool", "doesn't match", bad(good, func(m map[string][]byte) {
			m[characterName] = bytes.Replace(m[characterName], []byte("id: fern\n"), []byte("id: fern\ntools: [\"mcp:sandbox/run\"]\n"), 1)
		})},
		{"schema: avatar", "isn't allowed here", bad(good, func(m map[string][]byte) {
			m["sprig/avatar.json"] = []byte(`{"body":"round","script":"x"}`)
		})},
		{"skill id mismatch", "must match the file name", bad(good, func(m map[string][]byte) {
			m["skills/one.md"] = []byte("---\nid: two\nname: Two\ndescription: Use when\n---\n1. do\n")
		})},
		{"control characters", "control characters", bad(good, func(m map[string][]byte) {
			m["sprig/voice_tests/c.yaml"] = []byte("prompt: \"hi\x1b[31m\"\n")
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadGallery(bytes.NewReader(c.raw), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	// The baseline signed by the same helper is accepted.
	if _, err := ReadGallery(bytes.NewReader(signed(t, priv, "fern", good)), nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// A signature by a key whose id doesn't match.
	es := append([]entry(nil), orig...)
	i := idx(es, signatureName)
	var s Signature
	must(t, json.Unmarshal(es[i].data, &s))
	s.KeyID = "0123456789abcdef"
	es[i].data, _ = json.Marshal(s)
	if _, err := ReadGallery(bytes.NewReader(pack(t, es)), nil); err == nil || !strings.Contains(err.Error(), "key id") {
		t.Fatalf("key id mismatch: %v", err)
	}
}

func TestGalleryExportRefusesToolNames(t *testing.T) {
	in := newInstall(t)
	in.addFern(t, "")
	b, _ := os.ReadFile(filepath.Join(in.chars, "fern.md"))
	b = bytes.Replace(b, []byte("- mcp:gmail/*"), []byte("- mcp:gmail/send"), 1)
	must(t, os.WriteFile(filepath.Join(in.chars, "fern.md"), b, 0o644))
	lib, _ := in.db.Skills(context.Background())
	_, err := ExportGallery(GallerySource{CharsDir: in.chars, ID: "fern", Library: lib, LooksSecret: func(string) bool { return false }})
	if err == nil || !strings.Contains(err.Error(), "isn't a whole scope") {
		t.Fatalf("err = %v", err)
	}
}

func TestGalleryPrepareNeverKid(t *testing.T) {
	g := &Gallery{Char: &character.Character{ID: "fern", Name: "Fern", Role: "r", Household: character.TierKid}}
	if err := g.Prepare("", "", false); err == nil {
		t.Fatal("a kid sprig was prepared")
	}
}

func TestEmbeddedSchemasLoad(t *testing.T) {
	for _, n := range []string{"character", "avatar", "voicetest", "skill", "manifest", "signature"} {
		_ = loadSchema(n)
	}
	if _, err := compileSchema([]byte(`{"type":"object","oneOf":[]}`), "x"); err == nil {
		t.Fatal("an unsupported keyword was accepted")
	}
}

func FuzzReadGallery(f *testing.F) {
	seed, _, _ := NewSigningKey()
	priv, _ := ParseSigningKey(seed)
	c := &character.Character{ID: "fern", Name: "Fern", Role: "r", Personality: character.Personality{Traits: []string{"a"}}, Voice: character.Voice{Style: []string{"b"}}}
	g := &Gallery{Char: c, Files: map[string][]byte{"voice_tests/a.yaml": []byte("prompt: hi\n")},
		Skills: []store.Skill{{ID: "do-it", Name: "Do it", Description: "Use when", Body: "1. go"}}, Manifest: GalleryManifest{Kind: GalleryKind, Format: 1, ID: "fern", Name: "Fern"}}
	if raw, err := g.Bytes(priv); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})
	f.Add([]byte("\x1f\x8b\x08\x00"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		g, err := ReadGallery(bytes.NewReader(raw), nil)
		if err != nil {
			return
		}
		// Whatever is accepted is a valid, non-kid, non-host sprig whose
		// files are all on the allowlist.
		if g.Char == nil || g.Char.IsKid() || g.Char.Host || len(g.Char.Validate()) > 0 {
			t.Fatalf("accepted a bad sprig: %+v", g.Char)
		}
		for rel := range g.Files {
			if !galleryPath.MatchString(sprigDirPrefix + rel) {
				t.Fatalf("accepted %q", rel)
			}
		}
		_ = IsGallery(raw)
	})
}
