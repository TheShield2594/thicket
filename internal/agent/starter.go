package agent

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/theshield2594/thicket/internal/store"
)

// The starter pack is a handful of example skills written against
// Thicket's own tools. Nothing loads them automatically: the owner adds
// the ones they want with `thicket skill starter`, and can change them
// like any other skill.
//
//go:embed starterskills/*.skill.md
var starterFS embed.FS

// StarterSkills returns the bundled example skills, sorted by id.
func StarterSkills() ([]store.Skill, error) {
	paths, err := fs.Glob(starterFS, "starterskills/*.skill.md")
	if err != nil {
		return nil, err
	}
	var out []store.Skill
	for _, p := range paths {
		b, err := starterFS.ReadFile(p)
		if err != nil {
			return nil, err
		}
		k, err := ParseSkill(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

var (
	numberedStepRe = regexp.MustCompile(`(?m)^\s*\d+[.)]\s+\S`)
	whenRe         = regexp.MustCompile(`(?i)\b(use|when|whenever|before|after|if)\b`)
)

// skillHintLength is where a skill starts costing noticeably more each
// time it's read; past it we suggest splitting rather than refusing.
const skillHintLength = 3000

// SkillHints returns suggestions for a skill that is valid but could be
// easier for sprigs to follow. They never block a save.
func SkillHints(k store.Skill) []string {
	var h []string
	if !whenRe.MatchString(k.Description) {
		h = append(h, `say in the description when to use it ("Use when the user asks for…"): sprigs pick skills by their description`)
	}
	if !numberedStepRe.MatchString(k.Body) {
		h = append(h, "write the steps as a numbered list (1. 2. 3.), one action each")
	}
	if n := len(strings.TrimSpace(k.Body)); n > skillHintLength {
		h = append(h, fmt.Sprintf("the steps are %d characters; skills over %d are read in full every time, so consider splitting it into smaller skills", n, skillHintLength))
	}
	return h
}
