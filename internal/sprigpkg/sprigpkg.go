// Package sprigpkg makes shareable sprig packages: one file holding a
// sprig's character (soul, personality, voice), its routines, the skills
// it uses, its avatar settings and picture, and its voice tests, so it can
// be given to someone else's Thicket.
//
// What never goes in, whatever the sprig: memories, secrets and their
// values, conversations, the trail, inbox items, permission levels and
// integration credentials. Export only reads the files and rows listed
// here (an allowlist of paths), clears what belongs to this install from
// the character file (who it belongs to, kids and parents), and refuses
// to write the package if anything in it looks like a secret.
//
// Import checks the manifest and every file before anything is written,
// keeps only safe tool scopes, and the sprig starts at draft on every tool
// whatever the package says. Skills that exist with different content are
// saved as a new version of that skill, never overwritten.
package sprigpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"

	"gopkg.in/yaml.v3"
)

const (
	// Kind and Format identify the package format. A newer format is
	// refused with a message to update Thicket.
	Kind   = "thicket-sprig"
	Format = 1
	// Ext is the package file's extension (a gzipped tar).
	Ext = ".sprig"

	maxFileBytes   = 5 << 20
	maxTotalBytes  = 20 << 20
	maxFiles       = 200
	maxRoutines    = 20
	maxSkills      = 20
	manifestName   = "manifest.json"
	characterName  = "character.md"
	skillsName     = "skills.json"
	routinesName   = "routines.json"
	sprigDirPrefix = "sprig/"
)

// SafeScopes are the tool scopes an imported sprig keeps. Anything else
// the package asks for is listed so the person can grant it in chat.
var SafeScopes = []string{character.ScopeSchedule, character.ScopeWeb}

// avatarExts are the pictures export can read; a package always holds
// it re-encoded as avatar.png.
var avatarExts = []string{".png", ".jpg", ".jpeg", ".gif"}

const pictureName = "avatar.png"

// sprigFile matches the files a sprig's folder may contribute. Voice
// tests follow character.LoadVoiceTests: any *.yaml file.
var sprigFile = regexp.MustCompile(`^(avatar\.json|voice_tests/[A-Za-z0-9][A-Za-z0-9._-]{0,80}\.yaml)$`)

// allowed reports whether path may appear in a package.
func allowed(p string) bool {
	switch p {
	case manifestName, characterName, skillsName, routinesName, pictureName:
		return true
	}
	if rest, ok := strings.CutPrefix(p, sprigDirPrefix); ok {
		return sprigFile.MatchString(rest)
	}
	return false
}

// Manifest describes a package; Files holds each file's SHA-256.
type Manifest struct {
	Kind           string            `json:"kind"`
	Format         int               `json:"format"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	ThicketVersion string            `json:"thicket_version,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Files          map[string]string `json:"files"`
}

// Routine is a scheduled job the sprig runs (in the importer's timezone).
type Routine struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Task     string `json:"task"`
}

// Skill is a skill from the shared library that the sprig uses.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// Package is a sprig package in memory.
type Package struct {
	Manifest Manifest
	Char     *character.Character
	// Files are from the sprig's folder, relative to it: avatar.json and
	// voice_tests/*.yaml.
	Files map[string][]byte
	// Avatar is the picture, re-encoded as PNG (no metadata).
	Avatar   []byte
	Skills   []Skill
	Routines []Routine
	// Omitted says what export left out and why (paused routines, a
	// picture it couldn't clean), for the person exporting.
	Omitted []string
}

// Source is what Export reads.
type Source struct {
	CharsDir string
	ID       string
	// Jobs are the install's jobs; the sprig's scheduled ones become its
	// routines (webhook jobs are tied to this install's URLs).
	Jobs []store.Job
	// Library is the skill library; the skills the sprig uses go in.
	Library []store.Skill
	// ExtraSkills are skill ids to include even if not mentioned.
	ExtraSkills []string
	Version     string
	// LooksSecret reports secret-shaped text (stored secrets' values and
	// credential patterns). Required.
	LooksSecret func(string) bool
	Now         time.Time
}

// Export builds a package for one sprig. It refuses (and writes nothing)
// if anything in it looks like a secret.
func Export(src Source) (*Package, error) {
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
		// A kid sprig is defined by this family's kids and parents.
		return nil, fmt.Errorf("%s is a kid sprig, which belongs to this household's kids and parents and can't be packaged", c.DisplayName())
	}
	// What belongs to this install stays here: a solo sprig goes out as
	// a family one, and the importer decides whose it is.
	c.BelongsTo, c.Kids, c.Parents, c.Allow = 0, nil, nil, nil
	c.Host, c.CreatedBy, c.Household = false, "", ""
	p := &Package{Char: c, Files: map[string][]byte{}}
	// An https avatar is kept as a link (imports keep those too); a local
	// picture travels re-encoded, without its metadata.
	if c.Avatar != "" && !strings.HasPrefix(c.Avatar, "https://") {
		ext := strings.ToLower(filepath.Ext(c.Avatar))
		img, err := os.ReadFile(filepath.Join(src.CharsDir, filepath.Base(c.Avatar)))
		switch {
		case err != nil:
		case !slices.Contains(avatarExts, ext) || len(img) > maxFileBytes:
			p.Omitted = append(p.Omitted, "the picture "+c.Avatar+" (only PNG, JPEG or GIF up to 5 MB)")
		default:
			if clean, err := cleanPicture(img); err != nil {
				p.Omitted = append(p.Omitted, "the picture "+c.Avatar+": "+err.Error())
			} else {
				p.Avatar = clean
			}
		}
		c.Avatar = ""
	}
	dir := filepath.Join(src.CharsDir, c.ID)
	if b, err := os.ReadFile(filepath.Join(dir, "avatar.json")); err == nil {
		p.Files["avatar.json"] = b
	}
	tests, _ := filepath.Glob(filepath.Join(dir, "voice_tests", "*.yaml"))
	for _, f := range tests {
		rel := "voice_tests/" + filepath.Base(f)
		if !sprigFile.MatchString(rel) {
			p.Omitted = append(p.Omitted, "the voice test "+filepath.Base(f)+" (its file name has unusual characters)")
			continue
		}
		if b, err := os.ReadFile(f); err == nil {
			p.Files[rel] = b
		}
	}
	for _, j := range src.Jobs {
		if j.Character != c.ID || j.Kind != store.JobCron || j.Schedule == "" {
			continue
		}
		if !j.Enabled {
			// Imported routines run: a paused one stays here.
			p.Omitted = append(p.Omitted, "the paused routine "+j.Name)
			continue
		}
		p.Routines = append(p.Routines, Routine{Name: j.Name, Schedule: j.Schedule, Task: j.Prompt})
	}
	if len(p.Routines) > maxRoutines {
		return nil, fmt.Errorf("%s has %d routines; a package holds at most %d", c.DisplayName(), len(p.Routines), maxRoutines)
	}
	p.Skills = usedSkills(c, p.Routines, src.Library, src.ExtraSkills)
	if len(p.Skills) > maxSkills {
		return nil, fmt.Errorf("%s uses %d skills; a package holds at most %d", c.DisplayName(), len(p.Skills), maxSkills)
	}
	now := src.Now
	if now.IsZero() {
		now = time.Now()
	}
	p.Manifest = Manifest{Kind: Kind, Format: Format, ID: c.ID, Name: c.DisplayName(), ThicketVersion: src.Version, CreatedAt: now.UTC()}
	entries, err := p.entries()
	if err != nil {
		return nil, err
	}
	if err := checkLimits(entries); err != nil {
		return nil, err
	}
	var bad []string
	for name, data := range entries {
		if name == pictureName {
			continue // re-encoded pixels only: no text to leak
		}
		if src.LooksSecret(string(data)) {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("not exported: something that looks like a secret is in %s; remove it and try again", strings.Join(bad, ", "))
	}
	return p, nil
}

// skillRef finds a skill id mentioned as a whole word.
func mentions(text, id string) bool {
	re := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])` + regexp.QuoteMeta(id) + `($|[^a-z0-9_-])`)
	return re.MatchString(text)
}

// usedSkills are the library's skills named in the character file or a
// routine's task, plus extra.
func usedSkills(c *character.Character, routines []Routine, lib []store.Skill, extra []string) []Skill {
	md, _ := character.Marshal(c)
	text := string(md)
	for _, r := range routines {
		text += "\n" + r.Task
	}
	var out []Skill
	for _, k := range lib {
		if slices.Contains(extra, k.ID) || mentions(text, k.ID) {
			out = append(out, Skill{ID: k.ID, Name: k.Name, Description: k.Description, Body: k.Body})
		}
	}
	return out
}

// entries are the package's files (without the manifest), by path.
func (p *Package) entries() (map[string][]byte, error) {
	md, err := character.Marshal(p.Char)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{characterName: md}
	for rel, b := range p.Files {
		out[sprigDirPrefix+rel] = b
	}
	if p.Avatar != nil {
		out[pictureName] = p.Avatar
	}
	if len(p.Skills) > 0 {
		b, _ := json.MarshalIndent(p.Skills, "", "  ")
		out[skillsName] = b
	}
	if len(p.Routines) > 0 {
		b, _ := json.MarshalIndent(p.Routines, "", "  ")
		out[routinesName] = b
	}
	for name := range out {
		if !allowed(name) {
			return nil, fmt.Errorf("refusing to package %q", name)
		}
	}
	return out, nil
}

// checkLimits applies Read's limits before a package is written, so an
// export never makes a file import refuses. The manifest is allowed for.
func checkLimits(entries map[string][]byte) error {
	if len(entries)+1 > maxFiles {
		return fmt.Errorf("too many files (%d; a package holds at most %d): remove some voice tests", len(entries)+1, maxFiles)
	}
	total := 64 << 10 // room for the manifest
	for name, b := range entries {
		if len(b) > maxFileBytes {
			return fmt.Errorf("%s is too large (%d KB; at most %d MB per file)", name, len(b)>>10, maxFileBytes>>20)
		}
		total += len(b)
	}
	if total > maxTotalBytes {
		return fmt.Errorf("the package would be too large (%d MB; at most %d MB)", total>>20, maxTotalBytes>>20)
	}
	return nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Write writes the package as a gzipped tar: the manifest first.
func (p *Package) Write(w io.Writer) error {
	entries, err := p.entries()
	if err != nil {
		return err
	}
	if err := checkLimits(entries); err != nil {
		return err
	}
	p.Manifest.Files = map[string]string{}
	names := make([]string, 0, len(entries))
	for name, b := range entries {
		p.Manifest.Files[name] = sum(b)
		names = append(names, name)
	}
	sort.Strings(names)
	man, err := json.MarshalIndent(p.Manifest, "", "  ")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: p.Manifest.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := add(manifestName, man); err != nil {
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

// Read reads and checks a package: the format, that every file is allowed,
// listed in the manifest and matches its checksum, and that the character,
// avatar settings, voice tests, routines and skills are valid. Nothing is
// written.
func Read(r io.Reader) (*Package, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, maxTotalBytes+1<<20))
	if err != nil {
		return nil, errors.New("not a sprig package (it isn't a gzipped archive)")
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a sprig package: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("the package holds %q, which isn't a plain file", h.Name)
		}
		name := h.Name
		if !allowed(name) {
			return nil, fmt.Errorf("the package holds %q, which a sprig package can't contain", name)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("the package holds %q twice", name)
		}
		if len(files) >= maxFiles || h.Size > maxFileBytes || total+int(h.Size) > maxTotalBytes {
			return nil, errors.New("the package is too large")
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxFileBytes {
			return nil, errors.New("the package is too large")
		}
		total += len(b)
		files[name] = b
	}
	mb, ok := files[manifestName]
	if !ok {
		return nil, errors.New("not a sprig package: it has no manifest.json")
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("the manifest can't be read: %w", err)
	}
	if m.Kind != Kind {
		return nil, errors.New("not a sprig package (wrong kind in manifest.json)")
	}
	if m.Format > Format || m.Format < 1 {
		return nil, fmt.Errorf("this package is format %d; this Thicket reads format %d. Update Thicket to import it", m.Format, Format)
	}
	delete(files, manifestName)
	for name, b := range files {
		want, listed := m.Files[name]
		if !listed {
			return nil, fmt.Errorf("%s isn't listed in the manifest", name)
		}
		if want != sum(b) {
			return nil, fmt.Errorf("%s doesn't match its checksum: the package was changed or damaged", name)
		}
	}
	for name := range m.Files {
		if _, ok := files[name]; !ok {
			return nil, fmt.Errorf("the manifest lists %s, but it isn't in the package", name)
		}
	}
	p := &Package{Manifest: m, Files: map[string][]byte{}}
	cb, ok := files[characterName]
	if !ok {
		return nil, errors.New("the package has no character.md")
	}
	if p.Char, err = character.Parse(cb); err != nil {
		return nil, fmt.Errorf("character.md: %w", err)
	}
	if p.Char.ID != m.ID {
		return nil, fmt.Errorf("the manifest is for %q but character.md is %q", m.ID, p.Char.ID)
	}
	if probs := p.Char.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("character.md: %s", strings.Join(probs, "; "))
	}
	for name, b := range files {
		switch {
		case strings.HasPrefix(name, sprigDirPrefix):
			rel := strings.TrimPrefix(name, sprigDirPrefix)
			if err := checkSprigFile(rel, b); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			p.Files[rel] = b
		case name == pictureName:
			// Re-encoded here too: only pixels are ever written.
			clean, err := cleanPicture(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			p.Avatar = clean
		case name == skillsName:
			if err := json.Unmarshal(b, &p.Skills); err != nil {
				return nil, fmt.Errorf("skills.json: %w", err)
			}
		case name == routinesName:
			if err := json.Unmarshal(b, &p.Routines); err != nil {
				return nil, fmt.Errorf("routines.json: %w", err)
			}
		}
	}
	if len(p.Skills) > maxSkills || len(p.Routines) > maxRoutines {
		return nil, errors.New("the package has too many skills or routines")
	}
	for _, r := range p.Routines {
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Task) == "" {
			return nil, errors.New("routines.json: every routine needs a name and a task")
		}
		if _, err := scheduler.Parse(r.Schedule); err != nil {
			return nil, fmt.Errorf("routine %q: %w", r.Name, err)
		}
	}
	for _, k := range p.Skills {
		if strings.TrimSpace(k.ID) == "" {
			return nil, errors.New("skills.json: a skill has no id")
		}
	}
	return p, nil
}

func checkSprigFile(rel string, b []byte) error {
	if rel == "avatar.json" {
		var a avatar.Params
		if err := json.Unmarshal(b, &a); err != nil {
			return err
		}
		return a.Validate()
	}
	var vt character.VoiceTest
	if err := yaml.Unmarshal(b, &vt); err != nil {
		return err
	}
	if strings.TrimSpace(vt.Prompt) == "" {
		return errors.New("prompt is required")
	}
	return nil
}

// ReadFile reads a package from disk.
func ReadFile(name string) (*Package, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Prepare applies the import rules no package can override and an
// optional new id and name. It returns the scopes that were removed.
func (p *Package) Prepare(id, name string) (stripped []string, err error) {
	c := p.Char
	if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
		c.ID = id
	}
	if name = strings.TrimSpace(name); name != "" {
		c.Name, c.Nickname = name, ""
	}
	var kept []string
	for _, t := range c.Tools {
		if slices.Contains(SafeScopes, t) {
			kept = append(kept, t)
		} else {
			stripped = append(stripped, t)
		}
	}
	c.Tools = kept
	if c.Host {
		c.Host = false
		stripped = append(stripped, "host")
	}
	if c.IsKid() {
		return nil, errors.New("kid sprigs can't be imported from a package")
	}
	c.BelongsTo, c.Kids, c.Parents, c.Allow, c.Household = 0, nil, nil, nil, ""
	c.CreatedBy = "import"
	switch {
	case p.Avatar != nil:
		c.Avatar = c.ID + ".png"
	case !strings.HasPrefix(c.Avatar, "https://"):
		c.Avatar = "" // a file name from elsewhere would point at nothing
	}
	if probs := c.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(probs, "; "))
	}
	return stripped, nil
}

// Clash is why a package can't be imported under its id or name, with a
// free alternative to suggest.
type Clash struct {
	What, Value, Suggest string
}

func (c *Clash) Error() string {
	if c.Suggest == "" {
		return fmt.Sprintf("a sprig with the %s %q already exists; import it under another %s", c.What, c.Value, c.What)
	}
	return fmt.Sprintf("a sprig with the %s %q already exists; import it under another %s (for example %q)", c.What, c.Value, c.What, c.Suggest)
}

// Longest id and name a character may have (character.Validate).
const (
	maxIDLen   = 32
	maxNameLen = 40
)

// CheckClash refuses an id or name already taken (or being imported right
// now). nameTaken reports whether an id or name is in use.
func CheckClash(c *character.Character, charsDir string, nameTaken func(string) bool) error {
	idTaken := func(id string) bool {
		for _, f := range []string{id + ".md", id + lockSuffix} {
			if _, err := os.Stat(filepath.Join(charsDir, f)); err == nil {
				return true
			}
		}
		return nameTaken != nil && nameTaken(id)
	}
	if idTaken(c.ID) {
		return &Clash{What: "id", Value: c.ID, Suggest: freeName(c.ID, "-", maxIDLen, idTaken)}
	}
	if nameTaken != nil && nameTaken(c.Name) {
		return &Clash{What: "name", Value: c.Name, Suggest: freeName(c.Name, " ", maxNameLen, nameTaken)}
	}
	return nil
}

// freeName is base with the first free number added, cut to fit max;
// "" if none is free.
func freeName(base, sep string, max int, taken func(string) bool) string {
	for i := 2; i < 100; i++ {
		suffix := fmt.Sprintf("%s%d", sep, i)
		b := base
		if len(b)+len(suffix) > max {
			b = strings.TrimRight(b[:max-len(suffix)], "- ")
		}
		if s := b + suffix; !taken(s) {
			return s
		}
	}
	return ""
}

// SkillPlan is what importing a skill does.
type SkillPlan struct {
	Skill
	Action  string `json:"action"`            // "new", "same" (here and unchanged) or "new version"
	Base    int    `json:"base,omitempty"`    // the current version, for a new version
	Current string `json:"current,omitempty"` // the current version's body, for a new version
}

// PlanSkills compares the package's skills with the library. valid
// (agent.ValidateSkill) checks each one: a package with a skill that
// fails is refused before anything is shown or written.
func (p *Package) PlanSkills(ctx context.Context, db *store.Store, valid func(store.Skill) error) ([]SkillPlan, error) {
	var out []SkillPlan
	for _, k := range p.Skills {
		if valid != nil {
			if err := valid(store.Skill{ID: k.ID, Name: k.Name, Description: k.Description, Body: k.Body}); err != nil {
				return nil, fmt.Errorf("the skill %q in this package isn't valid: %w", k.ID, err)
			}
		}
		cur, err := db.SkillByID(ctx, k.ID, 0)
		switch {
		case errors.Is(err, store.ErrNotFound):
			out = append(out, SkillPlan{Skill: k, Action: "new"})
		case err != nil:
			return nil, err
		case cur.Name == k.Name && cur.Description == k.Description && strings.TrimSpace(cur.Body) == strings.TrimSpace(k.Body):
			out = append(out, SkillPlan{Skill: k, Action: "same"})
		default:
			out = append(out, SkillPlan{Skill: k, Action: "new version", Base: cur.Version, Current: cur.Body})
		}
	}
	return out, nil
}

// samePlan reports whether two plans would make the same changes.
func samePlan(a, b []SkillPlan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Action != b[i].Action || a[i].Base != b[i].Base {
			return false
		}
	}
	return true
}

// SkillsNeedAdmin is shown when someone who isn't an owner or admin
// imports a package with new or changed skills.
const SkillsNeedAdmin = "not imported: only an owner or admin can add or change shared skills"

// Preview is what a person reads before an import: every file the sprig
// gets (as a diff that creates it), its routines, and its skills in full
// (a changed skill as a diff from the current version). canWriteSkills is
// whether the importer may add or change shared skills.
func (p *Package) Preview(skills []SkillPlan, stripped []string, canWriteSkills bool) (string, error) {
	c := p.Char
	md, err := character.Marshal(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("```diff\n")
	b.WriteString(NewFileDiff(c.ID+".md", string(md)))
	rels := make([]string, 0, len(p.Files))
	for rel := range p.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		b.WriteString(NewFileDiff(c.ID+"/"+rel, string(p.Files[rel])))
	}
	b.WriteString("```\n")
	if p.Avatar != nil {
		fmt.Fprintf(&b, "Picture: characters/%s.png (%d KB)\n", c.ID, (len(p.Avatar)+1023)/1024)
	}
	if len(p.Routines) > 0 {
		b.WriteString("Routines (run in your timezone):\n")
		for _, r := range p.Routines {
			fmt.Fprintf(&b, "- %s `%s`: %s\n", r.Name, r.Schedule, r.Task)
		}
	}
	for _, k := range skills {
		switch {
		case k.Action == "same":
			fmt.Fprintf(&b, "Skill %s: already here, unchanged.\n", k.ID)
		case !canWriteSkills:
			fmt.Fprintf(&b, "Skill %s (%s): %s.\n", k.ID, k.Action, SkillsNeedAdmin)
		case k.Action == "new":
			fmt.Fprintf(&b, "Skill %s (new): %s\n```diff\n%s```\n", k.ID, k.Description, newLines("skills/"+k.ID, k.Body))
		default:
			fmt.Fprintf(&b, "Skill %s (new version after %d): %s\n```diff\n%s```\n", k.ID, k.Base, k.Description, replaceLines("skills/"+k.ID, k.Current, k.Body))
		}
	}
	if len(stripped) > 0 {
		fmt.Fprintf(&b, "Not given (grant in chat if you want them): %s\n", strings.Join(stripped, ", "))
	}
	b.WriteString("Every tool starts at draft. No memories, chats or secrets come with it.\n")
	return b.String(), nil
}

// NewFileDiff is a unified diff that creates path with content.
func NewFileDiff(name, content string) string {
	return newLines("characters/"+name, content)
}

func newLines(name, content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", name, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// replaceLines is a diff that replaces old with new, line for line.
func replaceLines(name, old, new string) string {
	ol := strings.Split(strings.TrimRight(old, "\n"), "\n")
	nl := strings.Split(strings.TrimRight(new, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", name, name, len(ol), len(nl))
	for _, l := range ol {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range nl {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// Installed is what Install did.
type Installed struct {
	Path     string
	Routines []int64
	Skills   []string // "id (new)", "id (version 3)", "id not imported: …"
}

// InstallOptions say who the sprig is installed for and what they saw.
type InstallOptions struct {
	// Owner runs the sprig's routines.
	Owner int64
	// Plans is the skill plan the person approved. If the library has
	// changed since, nothing is installed.
	Plans []SkillPlan
	// CanWriteSkills is whether the importer is an owner or admin; others
	// can't add or change shared skills.
	CanWriteSkills bool
}

// lockSuffix marks an id being imported, so two imports of one id can't
// both proceed.
const lockSuffix = ".importing"

// Install writes a prepared package: the sprig's folder files and picture,
// every tool at draft (levels left by an earlier sprig with this id are
// cleared), its routines, then the character file (the sprig goes live),
// then its skills. Anything that fails before the character file is
// undone. Files are created exclusively: nothing existing is replaced.
func (p *Package) Install(ctx context.Context, db *store.Store, charsDir string, opts InstallOptions) (*Installed, error) {
	c := p.Char
	if err := os.MkdirAll(charsDir, 0o755); err != nil {
		return nil, err
	}
	// Reserve the id first: a second import of it stops here.
	lock := filepath.Join(charsDir, c.ID+lockSuffix)
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%q is being imported already", c.ID)
		}
		return nil, err
	}
	lf.Close()
	defer os.Remove(lock)
	if _, err := os.Stat(filepath.Join(charsDir, c.ID+".md")); err == nil {
		return nil, &Clash{What: "id", Value: c.ID}
	}
	plans, err := p.PlanSkills(ctx, db, nil)
	if err != nil {
		return nil, err
	}
	if !samePlan(plans, opts.Plans) {
		return nil, errors.New("the skill library changed since the preview, so nothing was imported; run the import again to see what it would do now")
	}
	bg := context.WithoutCancel(ctx)
	var written []string
	var jobs []int64
	var oldLevels []store.ToolLevelRow
	levelSet := false
	undo := func(cause error) error {
		var left []string
		for _, id := range jobs {
			if err := db.DeleteJob(bg, id); err != nil {
				left = append(left, fmt.Sprintf("job #%d", id))
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
		os.Remove(filepath.Join(charsDir, c.ID, "voice_tests"))
		os.Remove(filepath.Join(charsDir, c.ID))
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
		written = append(written, full) // ours from here, even if the write fails
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	rels := make([]string, 0, len(p.Files))
	for rel := range p.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if !sprigFile.MatchString(rel) {
			return nil, undo(fmt.Errorf("refusing to write %q", rel))
		}
		if err := put(filepath.Join(charsDir, c.ID, filepath.FromSlash(rel)), p.Files[rel]); err != nil {
			return nil, undo(err)
		}
	}
	if p.Avatar != nil {
		if err := put(filepath.Join(charsDir, c.ID+".png"), p.Avatar); err != nil {
			return nil, undo(err)
		}
	}
	// Levels an earlier sprig with this id left behind would beat the
	// wildcard: clear them, so every tool really starts at draft.
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
	res := &Installed{}
	for _, r := range p.Routines {
		j, err := db.CreateJob(ctx, store.Job{UserID: opts.Owner, Character: c.ID, Name: r.Name, Schedule: r.Schedule, Prompt: r.Task, Kind: store.JobCron})
		if j != nil && j.ID != 0 {
			jobs = append(jobs, j.ID) // stored, even if reading it back failed
		}
		if err != nil {
			return nil, undo(fmt.Errorf("routine %q: %w", r.Name, err))
		}
	}
	md, err := character.Marshal(c)
	if err != nil {
		return nil, undo(err)
	}
	// Staged under a unique name and linked into place, which fails rather
	// than replace a character file that appeared meanwhile.
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
	res.Path, res.Routines = dest, jobs
	for _, k := range plans {
		if k.Action == "same" {
			continue
		}
		if !opts.CanWriteSkills {
			res.Skills = append(res.Skills, k.ID+" "+SkillsNeedAdmin)
			continue
		}
		sk := store.Skill{ID: k.ID, Name: k.Name, Description: k.Description, Body: k.Body, Author: "import", Note: "imported with " + c.DisplayName()}
		v, err := db.SaveSkill(ctx, sk, k.Base)
		switch {
		case errors.Is(err, store.ErrNoChange):
		case err != nil:
			res.Skills = append(res.Skills, fmt.Sprintf("%s not imported: %v", k.ID, err))
		case k.Action == "new":
			res.Skills = append(res.Skills, k.ID+" (new)")
		default:
			res.Skills = append(res.Skills, fmt.Sprintf("%s (version %d)", k.ID, v))
		}
	}
	return res, nil
}

// Bytes is the package as a file.
func (p *Package) Bytes() ([]byte, error) {
	var b bytes.Buffer
	err := p.Write(&b)
	return b.Bytes(), err
}
