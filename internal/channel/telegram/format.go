package telegram

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Sprigs write Discord-style Markdown. Telegram's Markdown modes are
// strict about escaping, so replies are converted to its small HTML
// subset instead: code blocks, inline code, bold, links and headings.
// Anything else stays as plain text.

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

var (
	boldRe    = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	linkRe    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	headingRe = regexp.MustCompile(`(?m)^#{1,6} +(.+)$`)
)

func toHTML(md string) string {
	var b strings.Builder
	for i, block := range strings.Split(md, "```") {
		if i%2 == 1 { // inside a fence; drop a language tag on its first line
			if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.ContainsAny(block[:nl], " \t") {
				block = block[nl+1:]
			}
			b.WriteString("<pre>" + escape(strings.Trim(block, "\n")) + "</pre>")
			continue
		}
		for j, span := range strings.Split(block, "`") {
			if j%2 == 1 {
				b.WriteString("<code>" + escape(span) + "</code>")
				continue
			}
			s := escape(span)
			s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
				p := linkRe.FindStringSubmatch(m)
				return `<a href="` + strings.ReplaceAll(p[2], `"`, "&quot;") + `">` + p[1] + "</a>"
			})
			s = boldRe.ReplaceAllString(s, "<b>$1</b>")
			s = headingRe.ReplaceAllString(s, "<b>$1</b>")
			b.WriteString(s)
		}
	}
	return b.String()
}

// split breaks text into chunks no longer than n bytes, preferring line
// breaks, never inside a code fence's backticks or a UTF-8 rune. A chunk
// that ends inside a code block closes it, and the next reopens it, so
// each chunk's formatting stands on its own.
func split(text string, n int) []string {
	const fence = "```"
	n -= len(fence) + 1 // room to close a fence at the end of a chunk
	var out []string
	for len(text) > n {
		cut := strings.LastIndex(text[:n], "\n")
		if cut < n/2 {
			cut = strings.LastIndex(text[:n], " ")
		}
		if cut < n/2 {
			cut = n
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		for cut > 0 && text[cut-1] == '`' && text[cut] == '`' { // inside a run of backticks
			cut--
		}
		chunk := strings.TrimSpace(text[:cut])
		rest := strings.TrimSpace(text[cut:])
		if strings.Count(chunk, fence)%2 == 1 { // cut inside a code block
			chunk += "\n" + fence
			rest = fence + "\n" + rest
		}
		out = append(out, chunk)
		text = rest
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func utf16Units(s string) []uint16 { return utf16.Encode([]rune(s)) }
func fromUTF16(u []uint16) string  { return string(utf16.Decode(u)) }
