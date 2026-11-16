package charimport

import (
	"strings"
	"testing"
)

func TestNormalizePlatform(t *testing.T) {
	for in, want := range map[string]string{
		"OpenClaw": "openclaw", "Clawdbot": "openclaw", "Hermes Agent": "hermes", "hermes_agent": "hermes",
		"SillyTavern": "card", "chub.ai": "card", "Grokbot": "grok", "xAI": "grok",
		"ChatGPT": "other", "": "other",
	} {
		if got := NormalizePlatform(in); got != want {
			t.Errorf("NormalizePlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuide(t *testing.T) {
	g := Guide("OpenClaw", true, false)
	if !strings.Contains(g, "thicket character import ~/.openclaw/workspace --from openclaw --memories") {
		t.Fatalf("openclaw guide:\n%s", g)
	}
	g = Guide("hermes", false, true)
	if !strings.Contains(g, "docker compose cp ~/.hermes thicket:/tmp/import-hermes") ||
		!strings.Contains(g, "docker compose exec thicket thicket character import /tmp/import-hermes --from hermes\n") {
		t.Fatalf("hermes docker guide:\n%s", g)
	}
	g = Guide("Grokbot", true, false)
	if !strings.Contains(g, "Grok can't export") || !strings.Contains(g, "Name: <your name>") ||
		!strings.Contains(g, "What you know about me") || !strings.Contains(g, "--from grok --memories") {
		t.Fatalf("grok guide:\n%s", g)
	}
	g = Guide("Character.AI", false, false)
	if !strings.Contains(g, "in Character.AI") || strings.Contains(g, "What you know about me") || !strings.Contains(g, "--from text\n") {
		t.Fatalf("other guide:\n%s", g)
	}
}

// A reply to ExportPrompt imports cleanly, memories included.
func TestExportReplyRoundTrip(t *testing.T) {
	reply := `Sure! Here I am:

**Name:** Ani
**Role:** Your playful late-night companion who keeps you company and hypes your wins.
**Traits:** bubbly, teasing, loyal, a little dramatic
**Voice:** Short, warm messages with lots of energy; teases but never mean.

## Background
I grew up on anime and arcade nights. I wear a black dress and twin tails.

## Example lines
- "You're up late again, huh?"
- "Okay but tell me EVERYTHING."

## What you know about me
- Your name is Sam.
- You're learning Japanese.
- <anything else>
`
	p := writeFile(t, t.TempDir(), "reply.md", reply)
	d, err := Load(p, Options{From: "grok"})
	if err != nil {
		t.Fatal(err)
	}
	c := d.Char
	if c.Name != "Ani" || c.ID != "ani" || c.Role != "Your playful late-night companion who keeps you company and hypes your wins." {
		t.Fatalf("got %q %q %q", c.Name, c.ID, c.Role)
	}
	if strings.Join(c.Personality.Traits, "|") != "bubbly|teasing|loyal|a little dramatic" {
		t.Fatalf("traits = %v", c.Personality.Traits)
	}
	if c.Voice.Style[0] != "Short, warm messages with lots of energy; teases but never mean." || !strings.Contains(c.Voice.Style[1], "stay in character") {
		t.Fatalf("voice = %v", c.Voice.Style)
	}
	if !strings.Contains(c.Body, "### Background") || !strings.Contains(c.Body, "### Example lines") || strings.Contains(c.Body, "learning Japanese") || strings.Contains(c.Body, "Name:") {
		t.Fatalf("body:\n%s", c.Body)
	}
	if len(d.Memories) != 2 || d.Memories[1].Content != "You're learning Japanese." || d.Memories[0].Personal {
		t.Fatalf("memories = %+v", d.Memories)
	}
	if len(d.Notes) != 0 {
		t.Fatalf("notes = %v", d.Notes)
	}
}
