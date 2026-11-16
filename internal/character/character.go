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
	ScopeMCPPrefix  = "mcp:"
)

type Personality struct {
	Traits       []string `yaml:"traits"`
	Quirks       []string `yaml:"quirks,omitempty"`
	Catchphrases []string `yaml:"catchphrases,omitempty"`
}

type Voice struct {
	Style    []string `yaml:"style"`
	NeverSay []string `yaml:"never_say,omitempty"`
}

type Relationships struct {
	User       string            `yaml:"user,omitempty"`
	Characters map[string]string `yaml:"characters,omitempty"`
}

type Initiative struct {
	CheckIns   bool   `yaml:"check_ins"`
	FollowUps  bool   `yaml:"follow_ups"`
	QuietHours string `yaml:"quiet_hours,omitempty"` // "22:00-08:00" in the user's timezone
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

	Body string `yaml:"-"` // Markdown after the frontmatter
	Path string `yaml:"-"`
	Hash string `yaml:"-"` // sha256 of the file bytes as loaded
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
	if n := len(c.Personality.Traits); n < 1 || n > 6 {
		p = append(p, fmt.Sprintf("personality.traits should list 1-6 core traits (has %d)", n))
	}
	if len(c.Voice.Style) == 0 {
		p = append(p, "voice.style needs at least one rule for how they talk")
	}
	if c.Color != "" && !colorRe.MatchString(c.Color) {
		p = append(p, fmt.Sprintf("color %q must look like #5aa469", c.Color))
	}
	for _, t := range c.Tools {
		switch {
		case t == ScopeSchedule, t == ScopeCharacters, t == ScopeAdmin, t == ScopeWeb:
		case mcpRe.MatchString(t):
		default:
			p = append(p, fmt.Sprintf("unknown tool scope %q (use schedule, web, characters, admin, or mcp:<server>/<tool|*>)", t))
		}
	}
	if c.Initiative.QuietHours != "" {
		if _, _, err := ParseQuietHours(c.Initiative.QuietHours); err != nil {
			p = append(p, err.Error())
		}
	}
	if len(c.Body) > 8000 {
		p = append(p, "the description below the frontmatter is too long (max 8000 characters)")
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
