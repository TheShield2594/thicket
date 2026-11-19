// Package character loads, validates and writes sprig character files.
//
// A character is one Markdown file with YAML frontmatter. The frontmatter
// holds the structured identity; the Markdown body is free-form backstory
// and example lines that go into the prompt as-is.
package character

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tool scopes a character can be granted. Core tools (memory, handoff,
// follow-ups) are always available and need no scope.
const (
	ScopeSchedule   = "schedule"
	ScopeCharacters = "characters"
	ScopeAdmin      = "admin"
	ScopeWeb        = "web"
	ScopeCalendar   = "calendar"
	ScopeMCPPrefix  = "mcp:"
)

// Household tiers. A solo sprig is one person's (belongs_to); a family
// sprig is shared by every adult; a kid sprig talks only to the kids it
// was set up for, and is locked down in code (see agent/kids.go).
const (
	TierSolo   = "solo"
	TierFamily = "family"
	TierKid    = "kid"
)

// What a parent can let a kid sprig see (Character.Allow).
const (
	AllowFamilyMemory = "family_memory"
	AllowCalendar     = "calendar"
	// AllowVoice lets a kid sprig answer out loud (spoken replies).
	AllowVoice = "voice"
)

// Person is someone who might talk to a sprig (store.User).
type Person interface {
	PersonID() int64
	Child() bool
}

type Personality struct {
	Traits       []string `yaml:"traits"`
	Quirks       []string `yaml:"quirks,omitempty"`
	Catchphrases []string `yaml:"catchphrases,omitempty"`
}

type Voice struct {
	Style    []string `yaml:"style"`
	NeverSay []string `yaml:"never_say,omitempty"`
	// SoundsLike is the speech voice spoken replies use (a voice name the
	// speech server knows, e.g. alloy). Empty: the speech_voice setting.
	SoundsLike string `yaml:"sounds_like,omitempty"`
	// Speaks is when the sprig answers out loud: never, when_spoken_to
	// (a voice message gets a voice message back; the default) or always.
	Speaks string `yaml:"speaks,omitempty"`
}

// When a sprig speaks its replies (Voice.Speaks).
const (
	SpeakNever        = "never"
	SpeakWhenSpokenTo = "when_spoken_to"
	SpeakAlways       = "always"
)

// SpeaksWhen is Voice.Speaks with its default filled in.
func (c *Character) SpeaksWhen() string {
	if c.Voice.Speaks == "" {
		return SpeakWhenSpokenTo
	}
	return c.Voice.Speaks
}

var voiceNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:+-]{1,64}$`)

type Relationships struct {
	User       string            `yaml:"user,omitempty"`
	Characters map[string]string `yaml:"characters,omitempty"`
}

type Initiative struct {
	CheckIns   bool   `yaml:"check_ins"`
	FollowUps  bool   `yaml:"follow_ups"`
	QuietHours string `yaml:"quiet_hours,omitempty"` // "22:00-08:00" in the user's timezone
	// DailyMessages caps the messages the sprig starts itself (job and
	// trigger output, check-ins, follow-ups) per person per day, in that
	// person's timezone. 0 means DefaultDailyMessages. Replies never count.
	DailyMessages int `yaml:"daily_messages,omitempty"`
}

// DefaultDailyMessages is the daily budget of proactive messages per
// person when initiative.daily_messages isn't set.
const DefaultDailyMessages = 3

// MaxDailyMessages is the highest initiative.daily_messages allowed.
const MaxDailyMessages = 50

// DailyBudget is how many proactive messages the sprig may send one person
// in a day.
func (c *Character) DailyBudget() int {
	if c.Initiative.DailyMessages > 0 {
		return c.Initiative.DailyMessages
	}
	return DefaultDailyMessages
}

type Character struct {
	ID            string        `yaml:"id"`
	Name          string        `yaml:"name"`
	Nickname      string        `yaml:"nickname,omitempty"`
	Role          string        `yaml:"role"`
	Host          bool          `yaml:"host,omitempty"`
	Avatar        string        `yaml:"avatar,omitempty"`
	Color         string        `yaml:"color,omitempty"`
	Personality   Personality   `yaml:"personality"`
	Voice         Voice         `yaml:"voice"`
	Relationships Relationships `yaml:"relationships,omitempty"`
	Tools         []string      `yaml:"tools,omitempty"`
	Initiative    Initiative    `yaml:"initiative,omitempty"`
	Model         string        `yaml:"model,omitempty"`
	CreatedBy     string        `yaml:"created_by,omitempty"`
	// BelongsTo makes a sprig one person's (their Thicket user id): only
	// they talk to it. 0, the default, is a sprig everyone shares.
	BelongsTo int64 `yaml:"belongs_to,omitempty"`
	// Household is the sprig's tier: solo, family or kid. Empty means solo
	// when belongs_to is set, else family.
	Household string `yaml:"household,omitempty"`
	// Kids (user ids) a kid sprig talks to, and the Parents (user ids) who
	// set it up: they can read its chats and get its safety alerts.
	Kids    []int64 `yaml:"kids,omitempty"`
	Parents []int64 `yaml:"parents,omitempty"`
	// Allow lists adult data a parent has let a kid sprig see:
	// family_memory, calendar, and voice (it may answer out loud). Nothing
	// by default.
	Allow []string `yaml:"allow,omitempty"`

	Body string `yaml:"-"` // Markdown after the frontmatter
	Path string `yaml:"-"`
	Hash string `yaml:"-"` // sha256 of the file bytes as loaded
}

// Tier is the sprig's household tier: solo, family or kid.
func (c *Character) Tier() string {
	switch {
	case c.Household == TierKid:
		return TierKid
	case c.BelongsTo != 0:
		return TierSolo
	}
	return TierFamily
}

// IsKid reports whether this is a kid sprig.
func (c *Character) IsKid() bool { return c.Tier() == TierKid }

// UsableBy reports whether a person may talk to this sprig: a family
// sprig by any adult, a solo sprig by its owner, a kid sprig by the kids
// it was set up for. Kids talk to kid sprigs only.
func (c *Character) UsableBy(p Person) bool {
	if p == nil {
		return false
	}
	switch c.Tier() {
	case TierKid:
		return p.Child() && slices.Contains(c.Kids, p.PersonID())
	case TierSolo:
		return !p.Child() && c.BelongsTo == p.PersonID()
	}
	return !p.Child()
}

// PrivateFrom reports whether this sprig is someone else's solo sprig, as
// seen by userID: then, admin or not, they may know it exists, what it
// costs and whether it's working, and nothing else (no name, role, tools,
// activity, trail, memories or chats). userID 0 is nobody in particular,
// so every solo sprig is private from it.
func (c *Character) PrivateFrom(userID int64) bool {
	return c.Tier() == TierSolo && c.BelongsTo != userID
}

// IsParent reports whether a user set up this kid sprig.
func (c *Character) IsParent(userID int64) bool {
	return c.IsKid() && slices.Contains(c.Parents, userID)
}

// Allows reports whether a parent let this kid sprig see something.
// Sprigs that aren't kid sprigs are not limited by it.
func (c *Character) Allows(what string) bool {
	return !c.IsKid() || slices.Contains(c.Allow, what)
}

// DisplayName is what users see: the nickname if there is one.

func (c *Character) DisplayName() string {
	if c.Nickname != "" {
		return c.Nickname
	}
	return c.Name
}

// Names returns every name the character answers to, lowercased.
func (c *Character) Names() []string {
	out := []string{strings.ToLower(c.Name)}
	if c.Nickname != "" && !strings.EqualFold(c.Nickname, c.Name) {
		out = append(out, strings.ToLower(c.Nickname))
	}
	return out
}

// HasScope reports whether the character was granted a tool scope.
func (c *Character) HasScope(scope string) bool {
	for _, t := range c.Tools {
		if t == scope {
			return true
		}
	}
	return false
}

// AllowsMCP reports whether the character may use tool on MCP server.
func (c *Character) AllowsMCP(server, tool string) bool {
	for _, t := range c.Tools {
		if !strings.HasPrefix(t, ScopeMCPPrefix) {
			continue
		}
		srv, name, ok := strings.Cut(strings.TrimPrefix(t, ScopeMCPPrefix), "/")
		if srv != server {
			continue
		}
		if !ok || name == "*" || name == tool {
			return true
		}
	}
	return false
}

// ColorInt parses Color ("#5aa469") for channel embeds; 0 if unset.
func (c *Character) ColorInt() int {
	s := strings.TrimPrefix(c.Color, "#")
	var v int
	if _, err := fmt.Sscanf(s, "%06x", &v); err != nil {
		return 0
	}
	return v
}

var (
	idRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	mcpRe   = regexp.MustCompile(`^mcp:[a-z0-9_-]+(/([A-Za-z0-9_.-]+|\*))?$`)
)

// Validate checks a character for problems. It returns every problem found,
// phrased so a user can fix the file.
func (c *Character) Validate() []string {
	var p []string
	if !idRe.MatchString(c.ID) {
		p = append(p, fmt.Sprintf("id %q must be 2-32 lowercase letters, digits or dashes, starting with a letter", c.ID))
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 40 {
		p = append(p, "name is required (max 40 characters)")
	}
	if len(c.Nickname) > 40 {
		p = append(p, "nickname is too long (max 40 characters)")
	}
	if strings.TrimSpace(c.Role) == "" || len(c.Role) > 200 {
		p = append(p, "role is required (one sentence, max 200 characters)")
	}
	if c.BelongsTo < 0 || (c.Host && c.BelongsTo != 0) {
		p = append(p, "belongs_to must be a user id, and the host is everyone's (leave it off)")
	}
	p = append(p, c.validateHousehold()...)
	if n := len(c.Personality.Traits); n < 1 || n > 6 {
		p = append(p, fmt.Sprintf("personality.traits should list 1-6 core traits (has %d)", n))
	}
	if len(c.Voice.Style) == 0 {
		p = append(p, "voice.style needs at least one rule for how they talk")
	}
	switch c.Voice.Speaks {
	case "", SpeakNever, SpeakWhenSpokenTo, SpeakAlways:
	default:
		p = append(p, fmt.Sprintf("voice.speaks %q must be never, when_spoken_to or always", c.Voice.Speaks))
	}
	if c.Voice.SoundsLike != "" && !voiceNameRe.MatchString(c.Voice.SoundsLike) {
		p = append(p, fmt.Sprintf("voice.sounds_like %q must be a speech voice name like alloy (letters, digits, - _ . only)", c.Voice.SoundsLike))
	}
	if c.Color != "" && !colorRe.MatchString(c.Color) {
		p = append(p, fmt.Sprintf("color %q must look like #5aa469", c.Color))
	}
	for _, t := range c.Tools {
		switch {
		case t == ScopeSchedule, t == ScopeCharacters, t == ScopeAdmin, t == ScopeWeb, t == ScopeCalendar:
		case mcpRe.MatchString(t):
		default:
			p = append(p, fmt.Sprintf("unknown tool scope %q (use schedule, web, calendar, characters, admin, or mcp:<server>/<tool|*>)", t))
		}
	}
	if c.Initiative.QuietHours != "" {
		if _, _, err := ParseQuietHours(c.Initiative.QuietHours); err != nil {
			p = append(p, err.Error())
		}
	}
	if n := c.Initiative.DailyMessages; n < 0 || n > MaxDailyMessages {
		p = append(p, fmt.Sprintf("initiative.daily_messages must be between 1 and %d (leave it off for %d)", MaxDailyMessages, DefaultDailyMessages))
	}
	if len(c.Body) > 8000 {
		p = append(p, "the description below the frontmatter is too long (max 8000 characters)")
	}
	return p
}

// validateHousehold checks the tier fields. Kid sprigs are checked
// strictly: what a file can't grant here, code never offers either.
func (c *Character) validateHousehold() []string {
	var p []string
	switch c.Household {
	case "", TierFamily, TierSolo, TierKid:
	default:
		return []string{fmt.Sprintf("household %q must be solo, family or kid", c.Household)}
	}
	if c.Household == TierSolo && c.BelongsTo == 0 {
		p = append(p, "a solo sprig needs belongs_to (whose it is)")
	}
	if c.Household == TierFamily && c.BelongsTo != 0 {
		p = append(p, "a family sprig is everyone's: leave belongs_to off")
	}
	if !c.IsKid() {
		if len(c.Kids) > 0 || len(c.Parents) > 0 || len(c.Allow) > 0 {
			p = append(p, "kids, parents and allow are only for kid sprigs (household: kid)")
		}
		return p
	}
	if c.Host || c.BelongsTo != 0 {
		p = append(p, "a kid sprig can't be the host or belong to one person (use kids and parents)")
	}
	if len(c.Kids) == 0 || len(c.Parents) == 0 {
		p = append(p, "a kid sprig needs kids (who it talks to) and parents (who set it up), as user ids")
	}
	for _, id := range append(append([]int64{}, c.Kids...), c.Parents...) {
		if id <= 0 {
			p = append(p, "kids and parents must be user ids")
			break
		}
	}
	for _, k := range c.Kids {
		if slices.Contains(c.Parents, k) {
			p = append(p, "someone can't be both a kid and a parent of the same sprig")
			break
		}
	}
	for _, t := range c.Tools {
		srv, tool, ok := strings.Cut(strings.TrimPrefix(t, ScopeMCPPrefix), "/")
		switch {
		case t == ScopeWeb:
		case strings.HasPrefix(t, ScopeMCPPrefix) && ok && srv != "" && tool != "*":
		default:
			p = append(p, fmt.Sprintf("a kid sprig can only have tools a parent approved one by one: web, or mcp:<server>/<tool> (not %q)", t))
		}
	}
	for _, a := range c.Allow {
		if a != AllowFamilyMemory && a != AllowCalendar && a != AllowVoice {
			p = append(p, fmt.Sprintf("allow %q must be family_memory, calendar or voice", a))
		}
	}
	if c.Initiative.CheckIns || c.Initiative.FollowUps {
		p = append(p, "a kid sprig doesn't check in or follow up: set initiative off")
	}
	return p
}

// ParseQuietHours parses "22:00-08:00" into minutes after midnight.
func ParseQuietHours(s string) (start, end int, err error) {
	var h1, m1, h2, m2 int
	if _, err := fmt.Sscanf(strings.ReplaceAll(s, " ", ""), "%d:%d-%d:%d", &h1, &m1, &h2, &m2); err != nil ||
		h1 > 23 || h2 > 23 || m1 > 59 || m2 > 59 || h1 < 0 || h2 < 0 || m1 < 0 || m2 < 0 {
		return 0, 0, fmt.Errorf("initiative.quiet_hours %q must look like 22:00-08:00", s)
	}
	return h1*60 + m1, h2*60 + m2, nil
}

// InQuietHours reports whether minuteOfDay falls in the quiet window.
func (c *Character) InQuietHours(minuteOfDay int) bool {
	if c.Initiative.QuietHours == "" {
		return false
	}
	start, end, err := ParseQuietHours(c.Initiative.QuietHours)
	if err != nil || start == end {
		return false
	}
	if start < end {
		return minuteOfDay >= start && minuteOfDay < end
	}
	return minuteOfDay >= start || minuteOfDay < end
}

var fence = []byte("---")

// Parse reads a character file's bytes.
func Parse(data []byte) (*Character, error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, fence) {
		return nil, errors.New("file must start with a '---' frontmatter block")
	}
	rest := data[len(fence):]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, errors.New("frontmatter is not closed with '---'")
	}
	front := rest[:end]
	body := rest[end+len("\n---"):]
	var c Character
	dec := yaml.NewDecoder(bytes.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	c.Body = strings.TrimSpace(string(body))
	sum := sha256.Sum256(data)
	c.Hash = hex.EncodeToString(sum[:])
	return &c, nil
}

// Marshal renders a character back to file bytes.
func Marshal(c *Character) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	enc.Close()
	buf.WriteString("---\n\n")
	if c.Body != "" {
		buf.WriteString(c.Body)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

// HashBytes returns the sha256 used to detect user edits.
func HashBytes(b []byte) string {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
