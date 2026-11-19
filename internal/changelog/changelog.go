// Package changelog reads CHANGELOG.md: one "## " heading per release,
// newest first. A released section's heading starts with its version
// ("## v0.2.0: households"); anything else ("## Unreleased: ...") is not
// yet released.
package changelog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Section is one "## " block.
type Section struct {
	Version string // "0.2.0", or "" for an unreleased section
	Heading string // the heading line without "## "
	Body    string // everything under it, trimmed
}

var versionRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)\b`)

// Parse splits the changelog into its sections, in file order.
func Parse(text string) []Section {
	var out []Section
	var cur *Section
	var body []string
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			out = append(out, *cur)
		}
		body = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			cur = &Section{Heading: strings.TrimSpace(h)}
			if m := versionRe.FindStringSubmatch(cur.Heading); m != nil {
				cur.Version = m[1]
			}
			continue
		}
		body = append(body, line)
	}
	flush()
	return out
}

// Normalize strips a leading "v": tags are v0.2.0, headings may be either.
func Normalize(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Notes returns the section for one released version, or an error naming
// what to add when there is none.
func Notes(text, version string) (Section, error) {
	want := Normalize(version)
	for _, s := range Parse(text) {
		if s.Version == want && want != "" {
			if s.Body == "" {
				return s, fmt.Errorf("the CHANGELOG.md section for %s is empty", want)
			}
			return s, nil
		}
	}
	return Section{}, fmt.Errorf("CHANGELOG.md has no section for %s: add a heading \"## v%s\" above the notes for this release", want, want)
}

// Between returns the released sections newer than from, up to and
// including to, newest first. An empty or unparseable from (a "dev"
// build) means everything up to to; an empty to means everything newer
// than from, with the unreleased sections above the newest release.
func Between(text, from, to string) []Section {
	from, to = Normalize(from), Normalize(to)
	_, fromOK := parse(from)
	_, toOK := parse(to)
	var out []Section
	released := false
	for _, s := range Parse(text) {
		if s.Version == "" {
			// Only what sits above the newest release is still to come.
			if !toOK && !released {
				out = append(out, s)
			}
			continue
		}
		released = true
		if toOK && Compare(s.Version, to) > 0 {
			continue
		}
		if fromOK && Compare(s.Version, from) <= 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Compare orders two versions: -1, 0 or 1. A version that doesn't parse
// (such as "dev" or a commit) sorts below every release. A pre-release
// sorts below its release.
func Compare(a, b string) int {
	pa, okA := parse(Normalize(a))
	pb, okB := parse(Normalize(b))
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] < pb.n[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	case pa.pre < pb.pre:
		return -1
	}
	return 1
}

// IsRelease reports whether v is a version like 1.2.3 or v1.2.3-rc.1.
func IsRelease(v string) bool { _, ok := parse(Normalize(v)); return ok }

type semver struct {
	n   [3]int
	pre string
}

func parse(v string) (semver, bool) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil || m[0] != v {
		return semver{}, false
	}
	core, pre, _ := strings.Cut(m[1], "-")
	var s semver
	for i, p := range strings.Split(core, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return semver{}, false
		}
		s.n[i] = n
	}
	s.pre = pre
	return s, true
}
