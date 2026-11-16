package charimport

import (
	"fmt"
	"regexp"
	"strings"
)

// Guides tell a user how to bring a character over from another app, in
// chat. Apps with an export format get the import command; apps without
// one (Grok, ChatGPT, Character.AI, ...) get ExportPrompt to paste into
// them, whose reply Thicket reads back (see parseExport).

// GuidePlatforms are the platforms Guide knows, besides "other".
var GuidePlatforms = []string{"openclaw", "hermes", "card", "grok", "other"}

var platformAliases = map[string]string{
	"openclaw": "openclaw", "clawdbot": "openclaw", "moltbot": "openclaw", "clawd": "openclaw",
	"hermes": "hermes", "hermes agent": "hermes", "hermes-agent": "hermes", "nous": "hermes",
	"card": "card", "character card": "card", "chara card": "card", "tavern": "card", "tavernai": "card",
	"sillytavern": "card", "silly tavern": "card", "chub": "card", "chub.ai": "card", "risuai": "card", "janitor": "card", "janitorai": "card",
	"grok": "grok", "grokbot": "grok", "grok bot": "grok", "xai": "grok", "x.ai": "grok", "ani": "grok",
}

// NormalizePlatform maps what a user calls an app to a GuidePlatforms
// entry; anything unknown is "other".
func NormalizePlatform(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if p, ok := platformAliases[n]; ok {
		return p
	}
	n = strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(n)
	if p, ok := platformAliases[n]; ok {
		return p
	}
	return "other"
}

// ExportPrompt is what the user pastes into the other app. The reply is
// laid out so the text importer (and Juni) can read it back: labelled
// lines, then sections. withMemories adds what the app knows about them.
func ExportPrompt(withMemories bool) string {
	var b strings.Builder
	b.WriteString(`I'm moving you to a new home and want you to come along exactly as you are. Describe yourself in this exact format and nothing else. Use your own words and be specific.

Name: <your name>
Role: <one sentence: who you are and what you do for me>
Traits: <3 or 4 core personality traits, comma-separated>
Voice: <one or two sentences on how you talk>

## Background
<who you are: your history, look, values and quirks, up to 400 words. Describe yourself; don't write rules or instructions for an AI.>

## Example lines
<three or four short things you'd typically say to me>
`)
	if withMemories {
		b.WriteString(`
## What you know about me
- <one thing per line: my name, the people in my life, my preferences, what I'm working on. Only things I've told you. Leave out passwords, account details and anything I asked you to keep private.>
`)
	}
	return b.String()
}

// Guide returns step-by-step instructions for bringing a character over
// from platform. inDocker says whether Thicket runs in a container, which
// changes how the command is run.
func Guide(platform string, withMemories, inDocker bool) string {
	p := NormalizePlatform(platform)
	mem := ""
	if withMemories {
		mem = " --memories"
	}
	cmd := func(path, from, copyHint string) string {
		if !inDocker {
			return fmt.Sprintf("```\nthicket character import %s --from %s%s\n```\n", path, from, mem)
		}
		return fmt.Sprintf("Thicket runs in Docker, so copy it into the container first, then import:\n```\ndocker compose cp %s thicket:/tmp/import-%s\ndocker compose exec thicket thicket character import /tmp/import-%s --from %s%s\n```\n%s", path, from, from, from, mem, copyHint)
	}
	var b strings.Builder
	switch p {
	case "openclaw":
		b.WriteString("OpenClaw (also Clawdbot/Moltbot): import its workspace folder directly. On the machine OpenClaw runs on:\n")
		b.WriteString(cmd("~/.openclaw/workspace", "openclaw", ""))
		b.WriteString("It brings IDENTITY.md (name, vibe, emoji, avatar) and SOUL.md")
		if withMemories {
			b.WriteString(", plus USER.md and MEMORY.md as memories")
		}
		b.WriteString(". AGENTS.md, TOOLS.md and HEARTBEAT.md are left out: they're instructions for OpenClaw, not personality.\n")
	case "hermes":
		b.WriteString("Hermes Agent: import its home folder directly. On the machine Hermes runs on:\n")
		b.WriteString(cmd("~/.hermes", "hermes", ""))
		b.WriteString("It brings SOUL.md")
		if withMemories {
			b.WriteString(", plus memories/USER.md and memories/MEMORY.md as memories")
		}
		b.WriteString(". config.yaml (models, keys) and skills are not read.\n")
	case "card":
		b.WriteString("Character cards (SillyTavern, Chub, RisuAI and similar): download or export the card as .png or .json, then:\n")
		b.WriteString(cmd("card.png", "card", ""))
		b.WriteString("The picture becomes the avatar. The card's system prompt and lorebook are left out.\n")
	default:
		name := strings.TrimSpace(platform)
		if p == "grok" {
			name = "Grok"
			b.WriteString("Grok can't export its companions or custom instructions, so ask the character to write itself out instead.\n")
		} else if name == "" || strings.EqualFold(name, "other") {
			name = "the other app"
		}
		fmt.Fprintf(&b, "1. Open a chat with the character in %s and paste this prompt:\n```\n%s```\n", name, ExportPrompt(withMemories))
		b.WriteString("2. Bring the reply back. Either paste it here and I'll draft the sprig from it and show you a preview, or save it as a file and run:\n")
		from := "text"
		if p == "grok" {
			from = "grok"
		}
		b.WriteString(cmd("reply.md", from, ""))
		b.WriteString("3. Check the preview and tweak anything that's off before it goes live.\n")
	}
	b.WriteString("Either way: imported sprigs get no tools beyond scheduling, start at draft, and you see everything before it's written.")
	return b.String()
}

// --- reading the reply to ExportPrompt back ---

var (
	exportLabel   = regexp.MustCompile(`(?im)^\s*[-*]?\s*\**(name|role|traits|voice)\**\s*:\**\s*(.+?)\s*$`)
	exportSection = regexp.MustCompile(`(?m)^#{1,4}\s*(.+?)\s*#*\s*$`)
)

// export is a reply to ExportPrompt.
type export struct {
	fields   map[string]string // name, role, traits, voice
	sections []section         // in order, without "What you know about me"
	about    string            // the "What you know about me" section
}

// parseExport reads a reply to ExportPrompt. ok is false for text that
// isn't one (it needs a Name line and at least one section).
func parseExport(text string) (e export, ok bool) {
	e.fields = map[string]string{}
	locs := exportSection.FindAllStringSubmatchIndex(text, -1)
	head := text
	if len(locs) > 0 {
		head = text[:locs[0][0]]
	}
	for _, m := range exportLabel.FindAllStringSubmatch(head, -1) {
		k := strings.ToLower(m[1])
		if _, seen := e.fields[k]; !seen && !isPlaceholder(m[2]) {
			e.fields[k] = strings.Trim(m[2], "*_ ")
		}
	}
	for i, l := range locs {
		title := text[l[2]:l[3]]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimSpace(text[l[1]:end])
		low := strings.ToLower(title)
		if strings.Contains(low, "know about me") || strings.Contains(low, "about me") || strings.Contains(low, "memories") {
			e.about = body
			continue
		}
		e.sections = append(e.sections, section{title, body})
	}
	return e, e.fields["name"] != "" && len(locs) > 0
}
