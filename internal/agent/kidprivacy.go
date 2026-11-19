package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
)

// What a kid tells their kid sprig stays in the kid sandbox. Its memories
// are the kid's own, kid conversations never add to family memory
// (memory.Reviewer), and kid sprigs run no jobs. The one way out is a
// tool that reaches outside Thicket with arguments the model wrote: a
// web search, a page address, an integration call (a parent may allow
// those). Before a kid sprig sends one, its arguments are checked here,
// and refused if they carry anything that looks personal (an email
// address, a phone number, a street address) or a detail the kid shared
// in this conversation or that the sprig remembers about them (their
// full name, where they live, their school).

var (
	emailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
	// Seven or more digits, with the usual separators: a phone number.
	phoneRe = regexp.MustCompile(`\+?\d[\d\s().-]{5,}\d`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$|^\d{1,2}[./-]\d{1,2}[./-]\d{2,4}$`)
	// A house number and a street: "42 Elm Street", "7 oak rd".
	streetRe = regexp.MustCompile(`(?i)\b\d{1,5}[a-z]?\s+(?:[a-z][a-z'-]*\s+){1,4}(?:street|st|road|rd|avenue|ave|lane|ln|drive|dr|court|ct|close|way|boulevard|blvd|place|pl|crescent|terrace|grove|gardens|circle|cir|parkway|pkwy|highway|hwy)\b`)
	// A school's name: "Maple Grove Elementary", "St Mary's Primary School".
	schoolRe = regexp.MustCompile(`\b((?:[A-Z][\w'.-]*\s+){1,4}(?:Elementary|Primary|Middle|High|Junior|Secondary|Grammar|Academy|School|College|Preschool|Kindergarten))\b`)
	// What follows "I live in…", "my name is…", "I go to…".
	livesRe = regexp.MustCompile(`(?i)\b(?:i live|we live|my house is|my home is|i'm from|i am from)\s+(?:in|on|at|near|by)?\s*([a-z0-9][\w' -]{1,60})`)
	nameRe  = regexp.MustCompile(`(?i)\b(?:my (?:full |real )?name is|my (?:last name|surname) is|i'm called|i am called)\s+([a-z][\w'-]*(?:\s+[a-z][\w'-]*){0,2})`)
	goesRe  = regexp.MustCompile(`(?i)\b(?:i go to|my school is|i'm at|i am at|i attend)\s+([a-z][\w' -]{1,60})`)
)

// fillerWords end a captured phrase ("I live in Springfield too").
var fillerWords = map[string]bool{"too": true, "now": true, "really": true, "the": true, "a": true, "an": true, "and": true, "with": true, "by": true, "near": true, "in": true, "on": true, "at": true}

// stopWords never count as a detail on their own.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and but so or of in on at by to near next with our my your his her their is it its
		i we me us too very there here really called name live lives go goes house home street road school town city big small
		little new old one two this that what when where who park lane avenue drive court close way place crescent terrace
		grove gardens circle elementary primary middle high junior secondary grammar academy college preschool kindergarten
		saint north south east west upper lower`) {
		stopWords[w] = true
	}
}

// personalDetails finds what looks personal in what the kid said: whole
// phrases, and their significant words (normalised, see normDetail).
// kidName is the kid's first name, which alone isn't a detail.
func personalDetails(texts []string, kidName string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = normDetail(s)
		if len(s) < 3 || seen[s] || stopWords[s] || strings.EqualFold(s, kidName) {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	addWords := func(phrase string, skipFirst bool) {
		add(phrase)
		for i, w := range strings.Fields(normDetail(phrase)) {
			if (i == 0 && skipFirst) || len(w) < 4 || stopWords[w] {
				continue
			}
			add(w)
		}
	}
	for _, t := range texts {
		for _, re := range []*regexp.Regexp{emailRe, streetRe} {
			for _, m := range re.FindAllString(t, -1) {
				add(m)
			}
		}
		for _, m := range phoneRe.FindAllString(t, -1) {
			if d := digitsOf(m); len(d) >= 7 && !dateRe.MatchString(strings.TrimSpace(m)) {
				add(d)
			}
		}
		for _, m := range schoolRe.FindAllStringSubmatch(t, -1) {
			addWords(m[1], false)
		}
		for _, re := range []*regexp.Regexp{livesRe, goesRe} {
			for _, m := range re.FindAllStringSubmatch(t, -1) {
				addWords(cutClause(m[1]), false)
			}
		}
		for _, m := range nameRe.FindAllStringSubmatch(t, -1) {
			addWords(cutClause(m[1]), true) // their first name alone is fine; a surname isn't
		}
	}
	return out
}

// cutClause keeps a captured phrase up to the end of its clause.
func cutClause(s string) string {
	if i := strings.IndexAny(s, ".,;!?\n"); i >= 0 {
		s = s[:i]
	}
	words := strings.Fields(s)
	for i, w := range words {
		if lw := strings.ToLower(w); lw == "and" || lw == "but" || lw == "because" || lw == "so" || lw == "with" {
			words = words[:i]
			break
		}
	}
	for len(words) > 0 && fillerWords[strings.ToLower(words[len(words)-1])] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

// normDetail lowercases s and turns everything but letters and digits
// into single spaces, so "42 Elm St." and "42+elm+st" compare equal.
func normDetail(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// carriesDetails reports whether s holds something personal: anything
// that looks like an email address, phone number or street address, or
// one of details.
func carriesDetails(s string, details []string) bool {
	if emailRe.MatchString(s) || streetRe.MatchString(s) || streetRe.MatchString(normDetail(s)) {
		return true
	}
	for _, m := range phoneRe.FindAllString(s, -1) {
		if len(digitsOf(m)) >= 7 && !dateRe.MatchString(strings.TrimSpace(m)) {
			return true
		}
	}
	norm := " " + normDetail(s) + " "
	digits := digitsOf(s)
	for _, d := range details {
		if strings.Contains(norm, " "+d+" ") {
			return true
		}
		if len(d) >= 7 && digitsOf(d) == d && strings.Contains(digits, d) {
			return true
		}
	}
	return false
}

// redactDetails takes personal details out of text meant for someone
// else (a parent's report).
func redactDetails(text string, details []string) string {
	text = emailRe.ReplaceAllString(text, "[private]")
	text = streetRe.ReplaceAllString(text, "[private]")
	text = schoolRe.ReplaceAllString(text, "[private]")
	text = phoneRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(digitsOf(m)) >= 7 && !dateRe.MatchString(strings.TrimSpace(m)) {
			return "[private]"
		}
		return m
	})
	for _, d := range details {
		if digitsOf(d) == d {
			continue
		}
		re, err := regexp.Compile(`(?i)\b` + strings.ReplaceAll(regexp.QuoteMeta(d), " ", `\W+`) + `\b`)
		if err == nil {
			text = re.ReplaceAllString(text, "[private]")
		}
	}
	return text
}

// outbound reports whether a tool reaches outside Thicket with arguments
// the model wrote.
func outbound(tool *Tool) bool {
	return tool.Scope == character.ScopeWeb || strings.HasPrefix(tool.Scope, character.ScopeMCPPrefix) || strings.HasPrefix(tool.Name, "mcp_")
}

// kidOutbound refuses a kid sprig's outbound call that carries personal
// details. It returns what the model is told, or "" to go ahead.
func (e *Engine) kidOutbound(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) string {
	if !t.char.IsKid() || !outbound(tool) {
		return ""
	}
	var said []string
	if t.conv != nil {
		if msgs, err := e.DB.RecentMessages(ctx, t.conv.ID, 200); err == nil {
			for _, m := range msgs {
				if m.Role == "user" {
					said = append(said, m.Content)
				}
			}
		}
		// Pinned messages stay in the model's context however old they are.
		if pins, err := e.DB.PinnedMessages(ctx, t.conv.ID); err == nil {
			for _, m := range pins {
				if m.Role == "user" {
					said = append(said, m.Content)
				}
			}
		}
	}
	if mems, err := e.DB.VisibleMemories(ctx, t.user.ID, t.char.ID); err == nil {
		for _, m := range mems {
			said = append(said, m.Content)
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(t.user.Name), " ")
	details := personalDetails(said, first)
	var values []string
	var v any
	if json.Unmarshal(args, &v) == nil {
		argValues(v, &values)
	} else {
		values = []string{string(args)}
	}
	for _, val := range values {
		// Each value on its own, so a match never spans two arguments, and
		// decoded too, so "42%20Elm%20Street" is seen as what it is.
		if carriesDetails(val, details) || carriesDetails(decoded(val), details) {
			e.logActivity(ctx, "warn", "safety", t.char.ID, t.user.ID, "kept personal details in: refused "+tool.Name, "")
			return "Error: NOT sent. It would have carried personal details the kid shared (like their name, where they live, their school or a phone number) outside Thicket, and kid sprigs never send those anywhere. Try again without them, or answer without it."
		}
	}
	return ""
}

// argValues collects every string, number and boolean in v.
func argValues(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case float64, bool:
		*out = append(*out, fmt.Sprint(x))
	case []any:
		for _, it := range x {
			argValues(it, out)
		}
	case map[string]any:
		for _, it := range x {
			argValues(it, out)
		}
	}
}

// decoded undoes URL escaping (percent-encoding, and + for spaces), as
// many times as it changes something; s itself if it isn't escaped.
func decoded(s string) string {
	for range 3 {
		d, err := url.QueryUnescape(s)
		if err != nil || d == s {
			break
		}
		s = d
	}
	return s
}
