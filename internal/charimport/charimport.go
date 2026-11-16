// Package charimport turns characters from other AI companion tools into
// draft sprigs: character cards (chara_card v1/v2/v3, JSON or PNG),
// OpenClaw workspaces, Hermes Agent homes, Grok personas and plain
// instruction files, plus Thicket's own character files.
//
// Everything imported is untrusted text. A draft never carries tool scopes
// beyond "schedule" and is never the host; what it says about itself can't
// change that. Callers show the draft to the user and write it only once
// they agree (see Write).
package charimport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/theshield2594/thicket/internal/character"
)

// Formats Load understands. "auto" picks one from the file or folder.
var Formats = []string{"auto", "thicket", "card", "openclaw", "hermes", "grok", "text"}

// Memory is a fact the source app knew about its user.
type Memory struct {
	Personal bool   // just between the user and this sprig; otherwise shared with every sprig
	Kind     string // a memory kind: fact, preference, person, project, shared_history
	Content  string
}

// Draft is an imported character, not yet written.
type Draft struct {
	Source   string // human name of the format, e.g. "OpenClaw workspace"
	Char     *character.Character
	Memories []Memory
	// Avatar is an image to save next to the character file as
	// <id><AvatarExt> (a PNG card's picture, an OpenClaw avatar file).
	Avatar    []byte
	AvatarExt string
	Stripped  []string // powers removed for safety
	Notes     []string // what was left out or guessed, for the user
}

// Options adjust an import.
type Options struct {
	From string // a value from Formats; "" means auto
	ID   string // override the generated id
	Name string // override the detected name
}

// maxInput bounds any one file read during an import.
const maxInput = 8 << 20

// Load reads path (a file or a folder) and returns a validated draft.
func Load(path string, o Options) (*Draft, error) {
	from := strings.ToLower(strings.TrimSpace(o.From))
	if from == "" {
		from = "auto"
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if from == "auto" {
		from, err = detect(path, info)
		if err != nil {
			return nil, err
		}
	}
	var d *Draft
	switch from {
	case "thicket":
		d, err = loadThicket(path)
	case "card":
		d, err = loadCard(path)
	case "openclaw":
		d, err = loadOpenClaw(path)
	case "hermes":
		d, err = loadHermes(path)
	case "grok", "text":
		d, err = loadText(path, from)
	default:
		return nil, fmt.Errorf("unknown format %q (use one of: %s)", o.From, strings.Join(Formats, ", "))
	}
	if err != nil {
		return nil, err
	}
	if n := strings.TrimSpace(o.Name); n != "" {
		d.Char.Name = n
	}
	if id := strings.TrimSpace(o.ID); id != "" {
		d.Char.ID = strings.ToLower(id)
	} else if d.Char.ID == "" {
		d.Char.ID = Slug(d.Char.Name)
	}
	d.secure()
	if probs := d.Char.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("the imported character needs fixing: %s", strings.Join(probs, "; "))
	}
	return d, nil
}

// detect guesses the format of path.
func detect(path string, info os.FileInfo) (string, error) {
	if info.IsDir() {
		has := func(name string) bool {
			_, err := os.Stat(filepath.Join(path, name))
			return err == nil
		}
		switch {
		case has("IDENTITY.md") || has("AGENTS.md") || has("HEARTBEAT.md"):
			return "openclaw", nil
		case has(filepath.Join("workspace", "SOUL.md")) || has(filepath.Join("workspace", "IDENTITY.md")):
			return "openclaw", nil // ~/.openclaw itself
		case has("memories") || has("config.yaml") || has(".env") && has("SOUL.md"):
			return "hermes", nil
		case has("SOUL.md"):
			return "openclaw", nil
		}
		return "", fmt.Errorf("%s doesn't look like an OpenClaw workspace or a Hermes Agent home (no SOUL.md or IDENTITY.md); pass --from to say what it is", path)
	}
	b, err := readLimited(path)
	if err != nil {
		return "", err
	}
	switch {
	case isPNG(b):
		return "card", nil
	case strings.HasPrefix(strings.TrimSpace(string(b)), "{"):
		return "card", nil
	case strings.HasPrefix(strings.TrimLeft(string(b), "\ufeff"), "---"):
		if _, err := character.Parse(b); err == nil {
			return "thicket", nil
		}
	}
	return "text", nil
}

// secure applies the import rules no source can override.
func (d *Draft) secure() {
	c := d.Char
	var kept []string
	for _, t := range c.Tools {
		if t == character.ScopeSchedule {
			kept = append(kept, t)
		} else {
			d.Stripped = append(d.Stripped, t)
		}
	}
	c.Tools = kept
	if c.Host {
		c.Host = false
		d.Stripped = append(d.Stripped, "host")
	}
	c.CreatedBy = "import"
	if d.Avatar != nil {
		c.Avatar = c.ID + d.AvatarExt
	} else if c.Avatar != "" && !strings.HasPrefix(c.Avatar, "https://") {
		// A file name from somewhere else would point at nothing here.
		c.Avatar = ""
	}
	if c.Color == "" {
		c.Color = "#7a9e7e"
	}
}

// Write saves the draft into charsDir: the avatar first, the character
// file last (once it exists the sprig is live). It refuses to replace an
// existing sprig.
func Write(d *Draft, charsDir string) error {
	c := d.Char
	if err := os.MkdirAll(charsDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(charsDir, c.ID+".md")
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("a character with id %q already exists (%s); pass --id to pick another", c.ID, dest)
	}
	var avatarPath string
	if d.Avatar != nil {
		avatarPath = filepath.Join(charsDir, c.ID+d.AvatarExt)
		if _, err := os.Stat(avatarPath); err == nil {
			return fmt.Errorf("%s already exists; pass --id to pick another id", avatarPath)
		}
		if err := os.WriteFile(avatarPath, d.Avatar, 0o644); err != nil {
			return err
		}
	}
	out, err := character.Marshal(c)
	if err != nil {
		return err
	}
	// Write to a temp name and rename, so the registry never sees half a file.
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		if avatarPath != "" {
			os.Remove(avatarPath)
		}
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		if avatarPath != "" {
			os.Remove(avatarPath)
		}
		return err
	}
	return nil
}

// --- Thicket's own format ---

func loadThicket(path string) (*Draft, error) {
	b, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	c, err := character.Parse(b)
	if err != nil {
		return nil, err
	}
	return &Draft{Source: "Thicket character file", Char: c}, nil
}

// --- helpers shared by the importers ---

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxInput {
		return nil, fmt.Errorf("%s is too large to import (%d bytes; max %d)", path, info.Size(), maxInput)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a folder, not a file", path)
	}
	return io.ReadAll(io.LimitReader(f, maxInput))
}

// readOptional returns a file's text, or "" if it doesn't exist.
func readOptional(path string) (string, error) {
	b, err := readLimited(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return normalize(string(b)), nil
}

func normalize(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(s)
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a name into a valid character id.
func Slug(name string) string {
	s := strings.ToLower(strings.Map(func(r rune) rune {
		switch {
		case r < unicode.MaxASCII:
			return r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return foldAccent(r)
		}
		return '-'
	}, name))
	s = strings.Trim(slugStrip.ReplaceAllString(s, "-"), "-")
	for len(s) > 0 && (s[0] < 'a' || s[0] > 'z') {
		s = strings.TrimLeft(s[1:], "-")
	}
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if len(s) < 2 {
		s = "imported"
	}
	return s
}

// foldAccent maps common accented Latin letters to ASCII; anything else
// becomes a separator.
func foldAccent(r rune) rune {
	const from, to = "àáâãäåèéêëìíîïòóôõöùúûüñçýÿÀÁÂÃÄÅÈÉÊËÌÍÎÏÒÓÔÕÖÙÚÛÜÑÇÝ", "aaaaaaeeeeiiiiooooouuuuncyyAAAAAAEEEEIIIIOOOOOUUUUNCY"
	fr, tr := []rune(from), []rune(to)
	for i, x := range fr {
		if x == r {
			return tr[i]
		}
	}
	return '-'
}

// clip shortens s to max bytes at a word boundary.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexAny(cut, " \n"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

var sentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)

// firstSentence returns the first sentence of s, on one line.
func firstSentence(s string) string {
	s = strings.Join(strings.Fields(stripHeadings(s)), " ")
	if loc := sentenceEnd.FindStringIndex(s); loc != nil {
		s = s[:loc[0]+1]
	}
	return s
}

func stripHeadings(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// role makes a valid one-sentence role.
func role(candidate, fallback string) string {
	r := clip(firstSentence(candidate), 190)
	if r == "" {
		r = fallback
	}
	return r
}

// traitList splits a short personality summary ("warm, curious; a bit
// dramatic") into 1-6 traits. Prose doesn't split well, so long items
// mean it isn't a list and nothing is returned.
func traitList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := regexp.MustCompile(`[,;\n/|]+|\s+and\s+`).Split(s, -1)
	var out []string
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), "-*•.\"'")
		if p == "" {
			continue
		}
		if len(p) > 48 || strings.Count(p, " ") > 5 {
			return nil
		}
		out = append(out, p)
		if len(out) == 6 {
			break
		}
	}
	return out
}

// safeBody prepares imported text for the character file's background:
// the source's own headings are demoted to bold lines so they can't pose
// as sections of Thicket's prompt (which uses ## headings), and tags that
// look like Thicket's markers are defused.
func safeBody(s string) string {
	lines := strings.Split(normalize(s), "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			h := strings.TrimSpace(strings.TrimLeft(t, "#"))
			if h == "" {
				lines[i] = ""
			} else {
				lines[i] = "**" + h + "**"
			}
		}
	}
	out := strings.Join(lines, "\n")
	out = regexp.MustCompile(`(?i)</?\s*untrusted[^>]*>`).ReplaceAllString(out, "")
	out = regexp.MustCompile(`(?i)\[\s*thicket event\s*\]`).ReplaceAllString(out, "(event)")
	return strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n"))
}

// section is one titled part of an imported background.
type section struct{ title, text string }

// buildBody joins sections under ### headings, fitting the character
// file's 8000-character limit. Earlier sections win; anything cut is noted.
func (d *Draft) buildBody(intro string, secs []section) string {
	const limit = 7900
	var b strings.Builder
	if intro != "" {
		b.WriteString(intro + "\n")
	}
	for _, s := range secs {
		text := safeBody(s.text)
		if text == "" {
			continue
		}
		head := "\n### " + s.title + "\n"
		room := limit - b.Len() - len(head)
		if room < 200 {
			d.Notes = append(d.Notes, fmt.Sprintf("%q was left out: the background is full (max 8000 characters)", s.title))
			continue
		}
		if len(text) > room {
			text = clip(text, room-4)
			d.Notes = append(d.Notes, fmt.Sprintf("%q was shortened to fit the 8000-character background", s.title))
		}
		b.WriteString(head + text + "\n")
	}
	return strings.TrimSpace(b.String())
}

// defaultVoice is the voice rule for sources that don't spell one out.
func defaultVoice(name string, hasExamples bool) []string {
	if hasExamples {
		return []string{"Talk the way " + name + " does in the example dialogue in the background.", "Chat-length replies; stay in character."}
	}
	return []string{"Talk the way " + name + " is described in the background.", "Chat-length replies; stay in character."}
}

var placeholder = regexp.MustCompile(`^(\(.*\)|\[.*\]|<.*>|tbd|todo|n/?a|none|optional|-+)$`)

// isPlaceholder reports template filler like "*(pick something you like)*".
func isPlaceholder(s string) bool {
	s = strings.Trim(strings.ToLower(strings.TrimSpace(s)), " *_")
	return s == "" || placeholder.MatchString(s)
}

var (
	namePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\byour name is\s+\**([A-Z][\p{L}'-]+(?: [A-Z][\p{L}'-]+)?)`),
		regexp.MustCompile(`(?:^|[\n.!]\s*)\**You are\s+\**([A-Z][\p{L}'-]+(?: [A-Z][\p{L}'-]+)?)\**[,.!:\s]`),
		regexp.MustCompile(`(?:^|[\n.!]\s*)\**(?:I am|I'm)\s+\**([A-Z][\p{L}'-]+)\**[,.!:\s]`),
		regexp.MustCompile(`(?i)\bname\s*[:=]\**\s*\**([\p{L}][\p{L}'-]+(?: [\p{L}][\p{L}'-]+)?)`),
	}
	notNames = map[string]bool{"A": true, "An": true, "The": true, "Not": true, "Now": true, "Here": true, "Always": true, "Never": true, "Going": true, "Talking": true, "Speaking": true, "Playing": true, "Roleplaying": true, "Acting": true, "Helpful": true}
)

// guessName finds a name an instruction text gives its character.
func guessName(text string) string {
	for _, re := range namePatterns {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			n := strings.TrimSpace(m[1])
			first, _, _ := strings.Cut(n, " ")
			if notNames[first] || len(n) > 40 {
				continue
			}
			return n
		}
	}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "# ") {
			h := strings.TrimSpace(strings.TrimPrefix(l, "# "))
			if h != "" && len(h) <= 40 && strings.Count(h, " ") <= 3 {
				return h
			}
		}
	}
	return ""
}

// nameFromFile turns "ani-persona.txt" into "Ani Persona".
func nameFromFile(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	words := strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '.' })
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return clip(strings.Join(words, " "), 40)
}

var labelLine = regexp.MustCompile(`^[^:]{1,40}:\s*(.*)$`)

// factLines splits a memory file into facts: bullets, "§"-separated
// entries (Hermes) or paragraphs. Headings and template filler are skipped.
func factLines(text string) []string {
	text = normalize(text)
	if text == "" {
		return nil
	}
	var chunks []string
	if strings.Contains(text, "§") {
		chunks = strings.Split(text, "§")
	} else {
		chunks = strings.Split(text, "\n\n")
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "-*•+ "))
		s = strings.Join(strings.Fields(strings.ReplaceAll(s, "**", "")), " ")
		if s == "" || isPlaceholder(s) || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "<!--") || seen[s] {
			return
		}
		// A "**Label:**" with nothing after it is template filler.
		if v := labelLine.FindStringSubmatch(s); v != nil && isPlaceholder(v[1]) {
			return
		}
		seen[s] = true
		out = append(out, clip(s, 500))
	}
	for _, c := range chunks {
		lines := strings.Split(strings.TrimSpace(c), "\n")
		bulleted := false
		for _, l := range lines {
			if t := strings.TrimSpace(l); strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "• ") {
				bulleted = true
			}
		}
		if bulleted {
			for _, l := range lines {
				add(l)
			}
			continue
		}
		var kept []string
		for _, l := range lines {
			if !strings.HasPrefix(strings.TrimSpace(l), "#") {
				kept = append(kept, l)
			}
		}
		add(strings.Join(kept, " "))
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// Remove undoes Write: it deletes the character file and the avatar.
func Remove(d *Draft, charsDir string) {
	os.Remove(filepath.Join(charsDir, d.Char.ID+".md"))
	if d.Avatar != nil {
		os.Remove(filepath.Join(charsDir, d.Char.ID+d.AvatarExt))
	}
}
