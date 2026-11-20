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
	// Only the version shown is approved.
	if _, err := ApprovePendingSkill(dst.chars, "fern", "seed-swap", []byte("---\nid: seed-swap\n---\nsomething else\n")); !errors.Is(err, ErrPendingChanged) {
		t.Fatalf("a different version was approved: %v", err)
	}
	shown, err := FormatSkillFile(pend[0])
	must(t, err)
	k, err := ApprovePendingSkill(dst.chars, "fern", pend[0].ID, shown)
	must(t, err)
	if k.ID != "seed-swap" {
		t.Fatalf("approved %s", k.ID)
	}
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
	if p, _ := up.Preview(trust); strings.Contains(p, "Every tool starts at draft") || !strings.Contains(p, "levels") {
		t.Fatalf("an update's preview promises draft:\n%s", p)
	}
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
	// Revoked after the preview, before the install: refused at install.
	must(t, dst.db.SetSetting(ctx, store.GalleryRevokedKey, up.Signer.KeyID))
	if _, err := up.Install(ctx, dst.db, dst.chars, GalleryInstallOptions{By: dst.owner.ID, Update: true}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("installed with a key revoked after the preview: %v", err)
	}
}

// An update whose files can't all be put in place leaves the sprig as it
// was.
func TestPublishAllRestores(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.yaml")
	must(t, os.WriteFile(first, []byte("old"), 0o644))
	blocked := filepath.Join(dir, "b.yaml")
	must(t, os.MkdirAll(filepath.Join(blocked, "x"), 0o755)) // a non-empty directory: rename onto it fails
	fresh := filepath.Join(dir, "new.md")
	err := publishAll([]stagedFile{{dest: first, data: []byte("new")}, {dest: fresh, data: []byte("n")}, {dest: blocked, data: []byte("b")}})
	if err == nil {
		t.Fatal("publishAll succeeded onto a directory")
	}
	if b, _ := os.ReadFile(first); string(b) != "old" {
		t.Fatalf("first file not restored: %q", b)
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("a new file was left behind")
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".gallery-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// A skill skipped for an id clash is still shown in full.
func TestPreviewShowsSkippedSkills(t *testing.T) {
	ctx := context.Background()
	src := newInstall(t)
	src.addFern(t, "")
	g := src.exportGallery(t)
	raw, err := g.Bytes(testKey(t))
	must(t, err)
	dst := newInstall(t)
	_, err = dst.db.SaveSkill(ctx, store.Skill{ID: "seed-swap", Name: "Mine", Description: "Use when mine", Body: "1. mine"}, 0)
	must(t, err)
	got, err := ReadGallery(bytes.NewReader(raw), nil)
	must(t, err)
	trust, err := got.CheckTrust(ctx, dst.db, dst.chars, nil)
	must(t, err)
	must(t, got.Prepare("", "", false))
	must(t, got.PlanSkills(ctx, dst.db, nil))
	p, err := got.Preview(trust)
	must(t, err)
	if !strings.Contains(p, "NOT imported) skills/seed-swap.md") || !strings.Contains(p, "2. offer them") {
		t.Fatalf("skipped skill not shown in full:\n%s", p)
	}
	if strings.Join(got.SkillIDs(), ",") != "watering-plan" {
		t.Fatalf("skills to install = %v", got.SkillIDs())
	}
}

// Export refuses what import would: a file over the per-file limit, and a
// manifest field too long.
func TestGalleryExportMatchesImportLimits(t *testing.T) {
	src := newInstall(t)
	src.addFern(t, "")
	vt := filepath.Join(src.chars, "fern", "voice_tests", "02-long.yaml")
	must(t, os.WriteFile(vt, []byte("prompt: hi\n# "+strings.Repeat("x", galleryMaxFileBytes)+"\n"), 0o644))
	lib, _ := src.db.Skills(context.Background())
	_, err := ExportGallery(GallerySource{CharsDir: src.chars, ID: "fern", Library: lib, Version: "test", LooksSecret: func(string) bool { return false }})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize file exported: %v", err)
	}
	must(t, os.Remove(vt))
	g := src.exportGallery(t)
	g.Manifest.ThicketVersion = strings.Repeat("9", 41)
	if _, err := g.Bytes(testKey(t)); err == nil {
		t.Fatal("signed a manifest import refuses")
	}
}

// Only one YAML document per file; nothing after the archive's end.
func TestGalleryStrictStream(t *testing.T) {
	if err := validateYAML([]byte("prompt: hi\n---\nsneaky: true\n"), "voicetest", "voice test"); err == nil {
		t.Fatal("a second YAML document passed")
	}
	if err := validateYAML([]byte("prompt: hi\n# just a comment\n"), "voicetest", "voice test"); err != nil {
		t.Fatalf("one document with a comment: %v", err)
	}
	priv := testKey(t)
	raw := signed(t, priv, "fern", goodFiles(t))
	if _, err := ReadGallery(bytes.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	// Data after the tar's end marker, inside the same gzip stream.
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	must(t, err)
	plain, err := io.ReadAll(gz)
	must(t, err)
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(append(plain, []byte("extra unsigned data")...))
	w.Close()
	if _, err := ReadGallery(bytes.NewReader(b.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "after its last file") {
		t.Fatalf("trailing data accepted: %v", err)
	}
	// A broken gzip ending (checksum) is refused too.
	bad := append([]byte(nil), raw...)
	bad[len(bad)-6] ^= 0xff
	if _, err := ReadGallery(bytes.NewReader(bad), nil); err == nil {
		t.Fatal("a corrupt gzip trailer was accepted")
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

// A rename that fails after earlier files were published puts them back;
// if putting back fails too, the error names the files and doesn't claim
// the sprig was left as it was.
func TestPublishAllRollsBackAfterARename(t *testing.T) {
	setup := func() (string, []stagedFile) {
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
		must(t, os.WriteFile(a, []byte("old a"), 0o644))
		must(t, os.WriteFile(b, []byte("old b"), 0o644))
		return dir, []stagedFile{{dest: a, data: []byte("new a")}, {dest: filepath.Join(dir, "c.md"), data: []byte("c")}, {dest: b, data: []byte("new b")}}
	}
	defer func(old func(string, string) error) { renameFile = old }(renameFile)

	// The third rename fails; the first two were published and are undone.
	calls := 0
	renameFile = func(from, to string) error {
		if calls++; calls == 3 {
			return errors.New("disk full")
		}
		return os.Rename(from, to)
	}
	dir, plan := setup()
	err := publishAll(plan)
	var rb *rollbackError
	if err == nil || errors.As(err, &rb) {
		t.Fatalf("want a plain error after a clean rollback: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.yaml")); string(b) != "old a" {
		t.Fatalf("published file not put back: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.md")); err == nil {
		t.Fatal("a new file was left behind")
	}

	// Every rename after the second fails: the rollback can't finish.
	calls = 0
	renameFile = func(from, to string) error {
		if calls++; calls >= 3 {
			return errors.New("disk full")
		}
		return os.Rename(from, to)
	}
	_, plan = setup()
	err = publishAll(plan)
	if !errors.As(err, &rb) || !strings.Contains(err.Error(), "check") {
		t.Fatalf("a failed rollback isn't reported: %v", err)
	}
}

// A stream that fills the size limit exactly still has its gzip trailer
// checked.
func TestGalleryTrailerCheckedAtTheLimit(t *testing.T) {
	priv := testKey(t)
	raw := signed(t, priv, "fern", goodFiles(t))
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	must(t, err)
	plain, err := io.ReadAll(gz)
	must(t, err)
	limit := galleryMaxTotal + 1<<20
	padded := append(plain, make([]byte, limit-len(plain))...)
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(padded)
	w.Close()
	if _, err := ReadGallery(bytes.NewReader(b.Bytes()), nil); err != nil {
		t.Fatalf("zero padding up to the limit refused: %v", err)
	}
	bad := append([]byte(nil), b.Bytes()...)
	bad[len(bad)-6] ^= 0xff // the CRC
	if _, err := ReadGallery(bytes.NewReader(bad), nil); err == nil {
		t.Fatal("a corrupt trailer at the size limit was accepted")
	}
}

// A skill goes back to waiting only as it was approved, and never over a
// newer pending version.
func TestRestorePendingSkill(t *testing.T) {
	dir := t.TempDir()
	k := store.Skill{ID: "seed-swap", Name: "Seed swap", Description: "Use when swapping seeds.", Body: "1. swap"}
	f, err := FormatSkillFile(k)
	must(t, err)
	p := pendingPath(dir, "fern", "seed-swap")
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	active := strings.TrimSuffix(p, PendingSuffix)
	must(t, os.WriteFile(active, f, 0o644))
	if err := RestorePendingSkill(dir, "fern", "seed-swap", []byte("something else")); !errors.Is(err, ErrRestoreChanged) {
		t.Fatalf("a different version was restored: %v", err)
	}
	must(t, os.WriteFile(p, []byte("newer"), 0o644))
	if err := RestorePendingSkill(dir, "fern", "seed-swap", f); !errors.Is(err, ErrRestoreChanged) {
		t.Fatalf("a newer pending version was overwritten: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "newer" {
		t.Fatal("pending file changed")
	}
	must(t, os.Remove(p))
	if err := RestorePendingSkill(dir, "fern", "seed-swap", f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("not back to waiting")
	}
	// It takes the sprig's lock.
	unlock, err := lockSprig(dir, "fern")
	must(t, err)
	defer unlock()
	if err := RestorePendingSkill(dir, "fern", "seed-swap", f); err == nil || errors.Is(err, ErrRestoreChanged) {
		t.Fatalf("restored without the lock: %v", err)
	}
}
