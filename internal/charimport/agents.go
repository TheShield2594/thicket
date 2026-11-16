package charimport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
)

// OpenClaw keeps an agent in a workspace folder (~/.openclaw/workspace):
// IDENTITY.md (name, creature, vibe, emoji, avatar), SOUL.md (persona,
// tone, boundaries), USER.md (about its human) and MEMORY.md (long-term
// memory). AGENTS.md, TOOLS.md, HEARTBEAT.md and BOOTSTRAP.md are operating
// instructions for OpenClaw itself, so they aren't imported.
//
// Hermes Agent keeps its persona in ~/.hermes/SOUL.md and its memory in
// ~/.hermes/memories/MEMORY.md (its own notes) and USER.md (about its
// user), with entries separated by "§".

func loadOpenClaw(path string) (*Draft, error) {
	dir := path
	if _, err := os.Stat(filepath.Join(dir, "SOUL.md")); err != nil {
		if _, err := os.Stat(filepath.Join(dir, "workspace", "SOUL.md")); err == nil {
			dir = filepath.Join(dir, "workspace")
		} else if _, err := os.Stat(filepath.Join(dir, "workspace", "IDENTITY.md")); err == nil {
			dir = filepath.Join(dir, "workspace")
		}
	}
	soul, err := readOptional(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return nil, err
	}
	identity, err := readOptional(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		return nil, err
	}
	if soul == "" && identity == "" {
		return nil, fmt.Errorf("%s has no SOUL.md or IDENTITY.md; point at the OpenClaw workspace (usually ~/.openclaw/workspace)", path)
	}
	id := parseIdentity(identity)
	d := &Draft{Source: "OpenClaw workspace"}
	name := id["name"]
	if name == "" {
		name = guessName(soul)
	}
	if name == "" {
		name = "Claw"
		d.Notes = append(d.Notes, "IDENTITY.md has no name, so it's called Claw; pass --name to change it")
	}
	name = clip(name, 40)
	c := &character.Character{
		Name:       name,
		Initiative: character.Initiative{FollowUps: true, QuietHours: "22:00-08:00"},
	}
	who := "Personal assistant"
	if cr := id["creature"]; cr != "" {
		who = clip(strings.ToUpper(cr[:1])+cr[1:], 80) + " and personal assistant"
	}
	c.Role = role(roleFromInstructions(soul, name), who+", brought over from OpenClaw.")
	c.Personality.Traits = traitList(id["vibe"])
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = traitsFromText(soul)
	}
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = []string{"true to the soul described in the background"}
		d.Notes = append(d.Notes, "no vibe in IDENTITY.md, so traits are a placeholder: tell Juni which 3-4 traits fit")
	}
	c.Voice.Style = defaultVoice(name, false)
	if e := id["emoji"]; e != "" {
		c.Personality.Quirks = []string{"signs off with " + clip(e, 16) + " now and then"}
	}
	var idSec []string
	for _, k := range []string{"creature", "vibe"} {
		if v := id[k]; v != "" {
			idSec = append(idSec, strings.ToUpper(k[:1])+k[1:]+": "+v)
		}
	}
	c.Body = d.buildBody("", []section{
		{"Identity", strings.Join(idSec, "\n")},
		{"Soul", soul},
	})
	d.Char = c
	if err := d.openClawAvatar(dir, id["avatar"]); err != nil {
		return nil, err
	}

	user, err := readOptional(filepath.Join(dir, "USER.md"))
	if err != nil {
		return nil, err
	}
	for _, f := range factLines(user) {
		d.Memories = append(d.Memories, Memory{Kind: "fact", Content: f})
	}
	mem, err := readOptional(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return nil, err
	}
	for _, f := range factLines(mem) {
		d.Memories = append(d.Memories, Memory{Personal: true, Kind: "fact", Content: f})
	}
	if daily, _ := filepath.Glob(filepath.Join(dir, "memory", "*.md")); len(daily) > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf("%d daily memory log(s) in memory/ were left out; only the curated MEMORY.md and USER.md come across", len(daily)))
	}
	var skipped []string
	for _, f := range []string{"AGENTS.md", "TOOLS.md", "HEARTBEAT.md", "BOOTSTRAP.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			skipped = append(skipped, f)
		}
	}
	if len(skipped) > 0 {
		d.Notes = append(d.Notes, "not imported, since it's operating instructions for OpenClaw rather than personality: "+strings.Join(skipped, ", ")+"; set up routines and tools in chat instead")
	}
	if _, err := os.Stat(filepath.Join(dir, "skills")); err == nil {
		d.Notes = append(d.Notes, "OpenClaw skills weren't imported; save the procedures you need as Thicket skills")
	}
	return d, nil
}

var identityLine = regexp.MustCompile(`^[-*•]?\s*\**\s*([A-Za-z ]{2,20}?)\s*:\s*\**\s*(.*)$`)

// parseIdentity reads IDENTITY.md's "- **Name:** value" lines, lowercasing
// the keys and dropping template filler.
func parseIdentity(text string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		m := identityLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(m[1]))
		v := strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "*_"))
		if isPlaceholder(v) {
			continue
		}
		if _, seen := out[k]; !seen {
			out[k] = v
		}
	}
	return out
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}

// openClawAvatar uses IDENTITY.md's avatar: an https URL as is, or an
// image file inside the workspace, copied over.
func (d *Draft) openClawAvatar(dir, avatar string) error {
	switch {
	case avatar == "":
		return nil
	case strings.HasPrefix(avatar, "https://"):
		d.Char.Avatar = avatar
		return nil
	case strings.HasPrefix(avatar, "http://") || strings.HasPrefix(avatar, "data:"):
		d.Notes = append(d.Notes, "the avatar isn't an https URL or a file, so it was left out")
		return nil
	}
	rel := filepath.Clean(filepath.FromSlash(avatar))
	ext := strings.ToLower(filepath.Ext(rel))
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") || !imageExts[ext] {
		d.Notes = append(d.Notes, "the avatar "+avatar+" isn't an image inside the workspace, so it was left out")
		return nil
	}
	b, err := readLimited(filepath.Join(dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		d.Notes = append(d.Notes, "the avatar file "+avatar+" wasn't found")
		return nil
	}
	if err != nil {
		return err
	}
	if ext == ".jpeg" {
		ext = ".jpg"
	}
	d.Avatar, d.AvatarExt = b, ext
	return nil
}

func loadHermes(path string) (*Draft, error) {
	soul, err := readOptional(filepath.Join(path, "SOUL.md"))
	if err != nil {
		return nil, err
	}
	d := &Draft{Source: "Hermes Agent"}
	name := guessName(soul)
	if name == "" {
		name = "Hermes"
	}
	name = clip(name, 40)
	c := &character.Character{
		Name:       name,
		Initiative: character.Initiative{FollowUps: true, QuietHours: "22:00-08:00"},
	}
	c.Role = role(roleFromInstructions(soul, name), "Personal assistant, brought over from Hermes Agent.")
	c.Personality.Traits = traitsFromText(soul)
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = []string{"true to the persona in the background"}
		d.Notes = append(d.Notes, "traits are a placeholder: tell Juni which 3-4 traits fit")
	}
	c.Voice.Style = defaultVoice(name, false)
	if soul == "" {
		d.Notes = append(d.Notes, "there's no SOUL.md, so "+name+" starts with Hermes's default persona: describe who they should be to Juni")
	}
	c.Body = d.buildBody("", []section{{"Persona", soul}})
	d.Char = c

	mem, err := readOptional(filepath.Join(path, "memories", "MEMORY.md"))
	if err != nil {
		return nil, err
	}
	user, err := readOptional(filepath.Join(path, "memories", "USER.md"))
	if err != nil {
		return nil, err
	}
	if soul == "" && mem == "" && user == "" {
		return nil, fmt.Errorf("%s has no SOUL.md or memories/; point at the Hermes home folder (usually ~/.hermes)", path)
	}
	for _, f := range factLines(user) {
		d.Memories = append(d.Memories, Memory{Kind: "fact", Content: f})
	}
	for _, f := range factLines(mem) {
		d.Memories = append(d.Memories, Memory{Personal: true, Kind: "fact", Content: f})
	}
	if _, err := os.Stat(filepath.Join(path, "skills")); err == nil {
		d.Notes = append(d.Notes, "Hermes skills weren't imported; save the procedures you need as Thicket skills")
	}
	if _, err := os.Stat(filepath.Join(path, "config.yaml")); err == nil {
		d.Notes = append(d.Notes, "config.yaml (model, tools, API keys) was not read")
	}
	return d, nil
}

// Grok, and anything else that's just instructions: Grok has no export
// for its companions or custom instructions, so paste the persona or the
// "Customize Grok" text into a file and import that.
func loadText(path, from string) (*Draft, error) {
	b, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	text := normalize(string(b))
	if text == "" {
		return nil, fmt.Errorf("%s is empty", path)
	}
	src, app := "instructions", "an instruction file"
	if from == "grok" {
		src, app = "Grok persona", "Grok"
	}
	d := &Draft{Source: src}
	if e, ok := parseExport(text); ok {
		return d.fromExport(e, app), nil
	}
	name := guessName(text)
	if name == "" {
		name = nameFromFile(path)
		d.Notes = append(d.Notes, "no name found in the text, so it's named after the file; pass --name to change it")
	}
	name = clip(name, 40)
	c := &character.Character{
		Name:       name,
		Initiative: character.Initiative{FollowUps: true, QuietHours: "22:00-08:00"},
	}
	c.Role = role(roleFromInstructions(text, name), name+", brought over from "+app+".")
	c.Personality.Traits = traitsFromText(text)
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = []string{"true to the instructions in the background"}
		d.Notes = append(d.Notes, "no trait list found, so traits are a placeholder: tell Juni which 3-4 traits fit")
	}
	c.Voice.Style = defaultVoice(name, false)
	c.Body = d.buildBody("", []section{{"Original instructions", text}})
	d.Char = c
	return d, nil
}

// fromExport builds a draft from a reply to ExportPrompt.
func (d *Draft) fromExport(e export, app string) *Draft {
	name := clip(e.fields["name"], 40)
	c := &character.Character{
		Name:       name,
		Initiative: character.Initiative{FollowUps: true, QuietHours: "22:00-08:00"},
	}
	c.Role = role(e.fields["role"], name+", brought over from "+app+".")
	c.Personality.Traits = traitList(e.fields["traits"])
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = []string{"true to the character in the background"}
		d.Notes = append(d.Notes, "the reply had no short trait list, so traits are a placeholder: tell Juni which 3-4 traits fit")
	}
	hasExamples := false
	for _, s := range e.sections {
		if strings.Contains(strings.ToLower(s.title), "example") && strings.TrimSpace(s.text) != "" {
			hasExamples = true
		}
	}
	c.Voice.Style = defaultVoice(name, hasExamples)
	if v := e.fields["voice"]; v != "" {
		c.Voice.Style = append([]string{clip(v, 300)}, c.Voice.Style[1:]...)
	}
	c.Body = d.buildBody("", e.sections)
	d.Char = c
	for _, f := range factLines(e.about) {
		d.Memories = append(d.Memories, Memory{Kind: "fact", Content: f})
	}
	return d
}

var (
	traitLabel = regexp.MustCompile(`(?im)\b(?:personality|traits|vibe)\**\s*:\**\s*(.+)$`)
	youAre     = regexp.MustCompile(`^\**(?:You are|You're)\s+`)
)

// traitsFromText finds a "Personality: a, b, c" line in instructions.
func traitsFromText(text string) []string {
	for _, m := range traitLabel.FindAllStringSubmatch(text, -1) {
		if t := traitList(m[1]); len(t) > 0 {
			return t
		}
	}
	return nil
}

// roleFromInstructions turns "You are Ani, a playful companion who..."
// into "A playful companion who...".
func roleFromInstructions(text, name string) string {
	s := strings.Trim(firstSentence(text), "*_ ")
	if loc := youAre.FindStringIndex(s); loc != nil {
		s = s[loc[1]:]
		s = strings.TrimPrefix(s, name)
		s = strings.TrimLeft(s, "*,:;- ")
		if s == "" || strings.HasPrefix(strings.ToLower(s), "not ") {
			return ""
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}
