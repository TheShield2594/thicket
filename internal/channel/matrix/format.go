package matrix

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Sprigs write Discord-style Markdown. Matrix clients render a small HTML
// subset (org.matrix.custom.html), so replies are sent as plain text in
// body and converted in formatted_body: code blocks, inline code, bold,
// links, headings and line breaks.

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
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
			b.WriteString("<pre><code>" + escape(strings.Trim(block, "\n")) + "</code></pre>")
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
				return `<a href="` + p[2] + `">` + p[1] + "</a>"
			})
			s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
			s = headingRe.ReplaceAllString(s, "<strong>$1</strong>")
			b.WriteString(strings.ReplaceAll(s, "\n", "<br>"))
		}
	}
	return b.String()
}

// split breaks text into chunks no longer than n bytes, preferring line
// breaks, never inside a UTF-8 rune. A chunk that ends inside a code
// block closes it, and the next reopens it.
func split(text string, n int) []string {
	const fence = "```"
	n -= len(fence) + 1
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
		for cut > 0 && text[cut-1] == '`' && text[cut] == '`' {
			cut--
		}
		if cut == 0 { // a run of backticks as long as the chunk: cut inside it
			cut = n
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		chunk, rest := splitAt(text, cut, fence)
		if len(rest) >= len(text) || chunk == "" {
			// Reopening a fence gave back all the text (a run of backticks
			// after an opening fence): cut at the limit, which always
			// makes progress.
			cut = n
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			chunk, rest = splitAt(text, cut, fence)
		}
		out = append(out, chunk)
		text = rest
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// splitAt cuts text at cut, closing an open code block in the first part
// and reopening it in the second.
func splitAt(text string, cut int, fence string) (chunk, rest string) {
	chunk = strings.TrimSpace(text[:cut])
	rest = strings.TrimSpace(text[cut:])
	if strings.Count(chunk, fence)%2 == 1 {
		chunk += "\n" + fence
		rest = fence + "\n" + rest
	}
	return chunk, rest
}

// stripReplyFallback removes the quoted "> <@user:server> ..." lines a
// client puts at the top of a reply's body (older clients still send
// them).
func stripReplyFallback(body string) string {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], ">") {
		i++
	}
	if i == 0 {
		return body
	}
	return strings.TrimSpace(strings.Join(lines[i:], "\n"))
}
