package charimport

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
)

// Character cards: the chara_card format companion and roleplay apps share.
// V1 keeps the fields at the top level; V2 ("chara_card_v2") and V3
// ("chara_card_v3") nest them under "data". A PNG card is the avatar with
// the JSON base64-encoded in a tEXt chunk named "chara" (V2) or "ccv3" (V3).

type cardData struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description"`
	Personality             string          `json:"personality"`
	Scenario                string          `json:"scenario"`
	FirstMes                string          `json:"first_mes"`
	MesExample              string          `json:"mes_example"`
	CreatorNotes            string          `json:"creator_notes"`
	SystemPrompt            string          `json:"system_prompt"`
	PostHistoryInstructions string          `json:"post_history_instructions"`
	AlternateGreetings      []string        `json:"alternate_greetings"`
	Tags                    []string        `json:"tags"`
	Nickname                string          `json:"nickname"` // V3
	CharacterBook           json.RawMessage `json:"character_book"`
}

type cardFile struct {
	Spec string   `json:"spec"`
	Data cardData `json:"data"`
	cardData
}

var pngSig = []byte("\x89PNG\r\n\x1a\n")

func isPNG(b []byte) bool { return bytes.HasPrefix(b, pngSig) }

func loadCard(path string) (*Draft, error) {
	b, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	d := &Draft{Source: "character card"}
	raw := b
	if isPNG(b) {
		raw, err = cardFromPNG(b)
		if err != nil {
			return nil, err
		}
		d.Source = "character card (PNG)"
		d.Avatar, d.AvatarExt = b, ".png"
	}
	var f cardFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("not a character card: %w", err)
	}
	data := f.cardData
	if strings.HasPrefix(f.Spec, "chara_card_v") || f.Data.Name != "" {
		data = f.Data
	}
	if strings.TrimSpace(data.Name) == "" {
		return nil, errors.New("not a character card: it has no name")
	}
	name := clip(strings.TrimSpace(data.Name), 40)
	fill := func(s string) string { return fillMacros(s, name) }

	c := &character.Character{
		Name:       name,
		Nickname:   clip(strings.TrimSpace(data.Nickname), 40),
		Initiative: character.Initiative{FollowUps: true, QuietHours: "22:00-08:00", DailyMessages: character.DefaultDailyMessages},
	}
	if c.Nickname == c.Name {
		c.Nickname = ""
	}
	c.Role = role(fill(data.Description), name+", brought over from a character card.")
	c.Personality.Traits = traitList(fill(data.Personality))
	personalityProse := ""
	if len(c.Personality.Traits) == 0 {
		personalityProse = fill(data.Personality)
		c.Personality.Traits = traitList(strings.Join(data.Tags, ", "))
	}
	if len(c.Personality.Traits) == 0 {
		c.Personality.Traits = []string{"true to the character in the background"}
		d.Notes = append(d.Notes, "no short trait list in the card, so traits are a placeholder: tell Juni which 3-4 traits fit")
	}
	examples := cleanExamples(fill(data.MesExample), name)
	c.Voice.Style = defaultVoice(name, examples != "")

	secs := []section{
		{"Description", fill(data.Description)},
		{"Personality", personalityProse},
		{"Scenario", fill(data.Scenario)},
		{"Example dialogue", examples},
		{"How " + name + " says hello", fill(data.FirstMes)},
	}
	c.Body = d.buildBody("", secs)
	d.Char = c

	if strings.TrimSpace(strings.ReplaceAll(data.SystemPrompt, "{{original}}", "")) != "" || strings.TrimSpace(data.PostHistoryInstructions) != "" {
		d.Notes = append(d.Notes, "the card's system prompt and post-history instructions were not imported (they're instructions for another app, not part of who the character is)")
	}
	if len(data.CharacterBook) > 0 && string(data.CharacterBook) != "null" {
		d.Notes = append(d.Notes, "the card's lorebook was not imported; ask the sprig to remember anything from it that matters")
	}
	if len(data.AlternateGreetings) > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf("%d alternate greeting(s) were left out", len(data.AlternateGreetings)))
	}
	return d, nil
}

var (
	charMacro = regexp.MustCompile(`(?i)\{\{\s*char\s*\}\}|<BOT>`)
	userMacro = regexp.MustCompile(`(?i)\{\{\s*user\s*\}\}|<USER>`)
	anyMacro  = regexp.MustCompile(`\{\{[^{}]*\}\}`)
)

// fillMacros resolves the {{char}} and {{user}} placeholders cards use.
func fillMacros(s, name string) string {
	s = charMacro.ReplaceAllLiteralString(s, name)
	s = userMacro.ReplaceAllLiteralString(s, "the user")
	return normalize(anyMacro.ReplaceAllString(s, ""))
}

// cleanExamples tidies example dialogue: <START> separators become blank
// lines and "the user:" speaker labels read as "User:".
func cleanExamples(s, name string) string {
	s = regexp.MustCompile(`(?i)<\s*start\s*>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?m)^the user:`).ReplaceAllString(s, "User:")
	return normalize(regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n"))
}

// cardFromPNG returns the card JSON embedded in a PNG, preferring V3.
func cardFromPNG(b []byte) ([]byte, error) {
	chunks := map[string]string{}
	p := b[len(pngSig):]
	for len(p) >= 12 {
		n := binary.BigEndian.Uint32(p[:4])
		typ := string(p[4:8])
		if uint64(n) > uint64(len(p)-12) {
			return nil, errors.New("the PNG is truncated")
		}
		body := p[8 : 8+n]
		switch typ {
		case "tEXt":
			if k, v, ok := bytes.Cut(body, []byte{0}); ok {
				chunks[strings.ToLower(string(k))] = string(v)
			}
		case "iTXt":
			// keyword \0 compression-flag compression-method lang \0 translated \0 text
			if k, rest, ok := bytes.Cut(body, []byte{0}); ok && len(rest) >= 2 && rest[0] == 0 {
				parts := bytes.SplitN(rest[2:], []byte{0}, 3)
				if len(parts) == 3 {
					chunks[strings.ToLower(string(k))] = string(parts[2])
				}
			}
		case "IEND":
			p = nil
			continue
		}
		p = p[12+n:]
	}
	for _, key := range []string{"ccv3", "chara"} {
		v, ok := chunks[key]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "{") {
			return []byte(v), nil
		}
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			dec, err = base64.RawStdEncoding.DecodeString(v)
		}
		if err != nil {
			return nil, fmt.Errorf("the PNG's %q card data isn't valid base64: %w", key, err)
		}
		return dec, nil
	}
	return nil, errors.New("this PNG has no character card in it (no \"chara\" or \"ccv3\" text chunk)")
}
