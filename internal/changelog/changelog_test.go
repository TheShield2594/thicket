package changelog

import (
	"strings"
	"testing"
)

const sample = `# Changelog

Intro.

## Unreleased: next things

- not out yet

## v0.3.0: households (schema v15)

- three

## 0.2.1

- two one

## v0.2.0

- two

## Unreleased: v0.1.0 (day one)

- old style heading, never released
`

func TestNotes(t *testing.T) {
	for _, v := range []string{"0.3.0", "v0.3.0"} {
		s, err := Notes(sample, v)
		if err != nil || s.Body != "- three" {
			t.Fatalf("Notes(%q) = %+v, %v", v, s, err)
		}
	}
	if _, err := Notes(sample, "v0.4.0"); err == nil || !strings.Contains(err.Error(), `"## v0.4.0"`) {
		t.Fatalf("missing section should say what to add, got %v", err)
	}
	// A heading that merely mentions a version is not that release.
	if _, err := Notes(sample, "0.1.0"); err == nil {
		t.Fatal("an Unreleased heading must not count as a release")
	}
	if _, err := Notes("## v1.0.0\n\n## v0.9.0\n- x\n", "1.0.0"); err == nil {
		t.Fatal("an empty section must fail")
	}
}

func versions(ss []Section) string {
	var v []string
	for _, s := range ss {
		if s.Version == "" {
			v = append(v, "unreleased")
		} else {
			v = append(v, s.Version)
		}
	}
	return strings.Join(v, ",")
}

func TestBetween(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"v0.2.0", "v0.3.0", "0.3.0,0.2.1"},
		{"0.2.1", "0.3.0", "0.3.0"},
		{"0.3.0", "0.3.0", ""},
		{"dev", "0.2.1", "0.2.1,0.2.0"},
		{"0.2.1", "", "unreleased,0.3.0"},
	}
	for _, c := range cases {
		if got := versions(Between(sample, c.from, c.to)); got != c.want {
			t.Errorf("Between(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0", "v0.2.0", 0},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"dev", "0.0.1", -1},
		{"abc123", "dev", 0},
		{"1.2.3", "1.2.4", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if IsRelease("dev") || !IsRelease("v1.2.3") || IsRelease("1.2") {
		t.Error("IsRelease")
	}
}
