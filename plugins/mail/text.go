package main

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Everything in this file turns outside content into inert text: no
// markup, nothing fetched, no control characters that could fake a line
// or reorder what a reader sees.

// htmlToText converts an HTML body to plain text. Scripts, styles and
// other non-content are dropped entirely; images become [image: alt] and
// are never fetched; links are written as `text <target>`.
func htmlToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// Never the raw source: it would carry scripts, hidden text and
		// tracking links straight to the sprig. (The parser refuses, for
		// example, nesting deeper than it allows.)
		return htmlUnreadable
	}
	var w textWriter
	w.node(doc)
	return tidy(w.b.String())
}

const htmlUnreadable = "[this message's HTML couldn't be read]"

// skipped elements: their content is never shown.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Template: true,
	atom.Noscript: true, atom.Iframe: true, atom.Object: true, atom.Embed: true,
	atom.Applet: true, atom.Frame: true, atom.Frameset: true, atom.Svg: true,
	atom.Math: true, atom.Title: true, atom.Select: true, atom.Button: true,
	atom.Textarea: true, atom.Input: true,
}

// blocks start and end on their own line.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true, atom.Ul: true, atom.Ol: true,
	atom.Li: true, atom.Table: true, atom.Tr: true, atom.Blockquote: true,
	atom.Pre: true, atom.Section: true, atom.Article: true, atom.Header: true,
	atom.Footer: true, atom.Hr: true, atom.Form: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Address: true, atom.Center: true,
	atom.Main: true, atom.Nav: true, atom.Aside: true, atom.Figure: true,
	atom.Figcaption: true, atom.Body: true, atom.Html: true, atom.Tbody: true,
	atom.Thead: true, atom.Tfoot: true, atom.Caption: true, atom.Fieldset: true,
	atom.Details: true, atom.Summary: true,
}

type textWriter struct {
	b     strings.Builder
	pre   int  // inside <pre>
	space bool // a space is pending
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		w.flushSpace()
		w.b.WriteString(s)
		return
	}
	if s != "" && unicode.IsSpace(firstRune(s)) {
		w.space = true
	}
	for i, f := range strings.FieldsFunc(s, unicode.IsSpace) {
		if i > 0 {
			w.space = true
		}
		w.flushSpace()
		w.b.WriteString(f)
	}
	if s != "" && unicode.IsSpace(lastRune(s)) {
		w.space = true
	}
}

func (w *textWriter) flushSpace() {
	if w.space {
		str := w.b.String()
		if str != "" && !strings.HasSuffix(str, "\n") && !strings.HasSuffix(str, " ") {
			w.b.WriteByte(' ')
		}
		w.space = false
	}
}

func (w *textWriter) newline() {
	w.space = false
	str := w.b.String()
	if str != "" && !strings.HasSuffix(str, "\n") {
		w.b.WriteByte('\n')
	}
}

func (w *textWriter) node(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.CommentNode, html.DoctypeNode:
		return
	case html.ElementNode:
		if skipped[n.DataAtom] {
			return
		}
		switch n.DataAtom {
		case atom.Br:
			w.b.WriteByte('\n')
			w.space = false
			return
		case atom.Img:
			alt := cleanLine(attr(n, "alt"))
			w.space = true // never glued to the text or image before it
			w.flushSpace()
			if alt == "" {
				w.b.WriteString("[image]")
			} else {
				w.b.WriteString("[image: " + alt + "]")
			}
			return
		case atom.A:
			w.link(n)
			return
		case atom.Td, atom.Th:
			w.space = true
		}
	}
	block := n.Type == html.ElementNode && blocks[n.DataAtom]
	if block {
		w.newline()
		if n.DataAtom == atom.Li {
			w.b.WriteString("- ")
		}
	}
	if n.DataAtom == atom.Pre {
		w.pre++
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.node(c)
	}
	if n.DataAtom == atom.Pre {
		w.pre--
	}
	if block {
		w.newline()
	}
}

// link writes `text <target>`. Only the real href counts, trimmed; a
// javascript:, vbscript: or data: target is never shown.
func (w *textWriter) link(n *html.Node) {
	var inner textWriter
	inner.pre = w.pre
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		inner.node(c)
	}
	text := strings.Join(strings.Fields(inner.b.String()), " ")
	href := strings.TrimSpace(attr(n, "href"))
	target := ""
	if href != "" && !strings.HasPrefix(href, "#") {
		// Browsers ignore tabs, newlines and controls inside a scheme
		// ("java\tscript:"), so look at it the same way.
		squashed := strings.ToLower(strings.Map(func(r rune) rune {
			if r <= ' ' || unicode.IsControl(r) || isInvisible(r) {
				return -1
			}
			return r
		}, href))
		if strings.HasPrefix(squashed, "javascript:") || strings.HasPrefix(squashed, "data:") || strings.HasPrefix(squashed, "vbscript:") {
			target = "link removed"
		} else {
			target = strings.NewReplacer(" ", "%20", "<", "%3C", ">", "%3E", "\"", "%22").Replace(cleanLine(href))
		}
	}
	w.flushSpace()
	switch {
	case target == "":
		w.b.WriteString(text)
	case text == "" || text == target:
		w.b.WriteString("<" + target + ">")
	default:
		w.b.WriteString(text + " <" + target + ">")
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func firstRune(s string) rune { r, _ := utf8.DecodeRuneInString(s); return r }
func lastRune(s string) rune  { r, _ := utf8.DecodeLastRuneInString(s); return r }

var manyBlankLines = regexp.MustCompile(`\n{3,}`)

// tidy trims each line and keeps at most one blank line in a row.
func tidy(s string) string {
	s = cleanText(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimLeft(l, " \t"), " \t")
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(manyBlankLines.ReplaceAllString(s, "\n\n"))
}

// isInvisible reports characters that change how text looks without
// being seen: bidi controls, zero-width characters, line separators.
func isInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E,
		r >= 0x2066 && r <= 0x2069, r == 0x2028, r == 0x2029, r == 0xFEFF, r == 0x061C:
		return true
	}
	return false
}

// cleanText keeps a body's lines and tabs but drops other control and
// invisible characters (and bad UTF-8).
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' {
			return '\n'
		}
		if unicode.IsControl(r) || isInvisible(r) {
			return -1
		}
		return r
	}, s)
}

// cleanLine makes a header value (subject, name) a single inert line.
func cleanLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		if isInvisible(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cleanName cleans a display name and removes what could make it look
// like a second address: angle brackets and quotes.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '"':
			return -1
		}
		return r
	}, s)
	return cleanLine(s)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// firstRunes is the first n characters of s.
func firstRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// One-time codes: a 4–8 digit code near a word that says what it is.
var (
	otpWord = regexp.MustCompile(`(?i)\b(codes?|verification|one[- ]time|otp|2fa|passcode|security code)\b`)
	otpCode = regexp.MustCompile(`\b\d{4,8}\b|\b\d{3}[- ]\d{3}\b`)
)

const (
	otpNear   = 120 // characters between the word and the code
	otpHidden = `[one-time code hidden: shown to the person in "needs you"]`
)

// maskOTP hides one-time codes in the subject and text, and reports
// whether there was one. A code counts when it is near one of the words
// in the same text, or in the start of the text when the subject has one.
func maskOTP(subject, text string) (string, bool) {
	subjectWord := otpWord.MatchString(subject)
	text, found := maskIn(text, subjectWord)
	_, inSubject := maskIn(subject, false)
	return text, found || inSubject
}

// MaskSubject is maskOTP for a subject alone.
func maskSubject(subject string) string {
	s, _ := maskIn(subject, false)
	return s
}

func maskIn(s string, nearStart bool) (string, bool) {
	words := otpWord.FindAllStringIndex(s, -1)
	if len(words) == 0 && !nearStart {
		return s, false
	}
	codes := otpCode.FindAllStringIndex(s, -1)
	var b strings.Builder
	last, found := 0, false
	// Codes and words both come in order, so one walk through each finds,
	// for every code, the last word ending before it and the first word
	// starting after it: linear, however many numbers and words there are.
	w := 0
	for _, c := range codes {
		if !standalone(s, c[0], c[1]) {
			continue
		}
		for w < len(words) && words[w][1] <= c[0] {
			w++
		}
		near := nearStart && c[0] < 500
		if w > 0 && c[0]-words[w-1][1] <= otpNear {
			near = true
		}
		after := w
		for after < len(words) && words[after][0] < c[1] {
			after++ // overlaps the code: neither before nor after it
		}
		if after < len(words) && words[after][0]-c[1] <= otpNear {
			near = true
		}
		if !near {
			continue
		}
		b.WriteString(s[last:c[0]])
		b.WriteString(otpHidden)
		last = c[1]
		found = true
	}
	if !found {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// digitsHidden replaces a number in a preview only the provider made.
const digitsHidden = "[number hidden]"

// maskDigits hides every number that could be a one-time code. It is for
// previews where the rest of the text (and so the word that would say it
// is a code) isn't known: better an order number hidden than a code shown.
func maskDigits(s string) string {
	var b strings.Builder
	last := 0
	for _, c := range otpCode.FindAllStringIndex(s, -1) {
		if !standalone(s, c[0], c[1]) {
			continue
		}
		b.WriteString(s[last:c[0]])
		b.WriteString(digitsHidden)
		last = c[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// previewsOf is the preview of a whole text, as is (for the person) and
// with one-time codes hidden (for the sprig).
func previewsOf(subject, text string) (plain, masked string) {
	m, _ := maskOTP(subject, text)
	return preview(text), preview(m)
}

// standalone: the digits aren't part of a longer number like a phone
// number, a date or an amount ("555-1234", "12.5000", "1,0000").
func standalone(s string, start, end int) bool {
	joiner := func(c byte) bool { return c == '-' || c == '.' || c == ',' || c == '/' || c == ':' }
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	if start >= 2 && joiner(s[start-1]) && digit(s[start-2]) {
		return false
	}
	if end+1 < len(s) && joiner(s[end]) && digit(s[end+1]) {
		return false
	}
	return true
}
