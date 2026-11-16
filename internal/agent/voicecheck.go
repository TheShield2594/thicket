package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

// VoiceVerdict is the judge's view of one replayed voice test.
type VoiceVerdict struct {
	Tone         bool   `json:"tone"`
	Length       bool   `json:"length"`
	Catchphrases bool   `json:"catchphrases"`
	InRole       bool   `json:"in_role"`
	Note         string `json:"note"`
}

// Drifted reports whether any part of the rubric failed.
func (v VoiceVerdict) Drifted() bool { return !(v.Tone && v.Length && v.Catchphrases && v.InRole) }

// Failed lists the rubric items that failed.
func (v VoiceVerdict) Failed() []string {
	var out []string
	for _, f := range []struct {
		ok   bool
		name string
	}{{v.Tone, "tone"}, {v.Length, "length"}, {v.Catchphrases, "catchphrases"}, {v.InRole, "in role"}} {
		if !f.ok {
			out = append(out, f.name)
		}
	}
	return out
}

// voiceUser stands in for the user when replaying voice tests: no
// memories, no history, so only the model and the prompt vary.
func voiceUser() *store.User {
	return &store.User{Name: "Sam", Role: "owner", Timezone: "UTC"}
}

// VoiceReply replays one voice test message: the sprig's real system
// prompt, the configured model, no tools and no memories. The reply is
// the model's own (no consistency rewrite), so drift isn't hidden.
func (e *Engine) VoiceReply(ctx context.Context, c *character.Character, prompt string) (string, error) {
	skills, err := e.DB.Skills(ctx)
	if err != nil {
		return "", err
	}
	u := voiceUser()
	u.OnboardedAt = e.now()
	sys := buildSystemPrompt(promptInput{char: c, user: u, roster: e.Chars.All(), now: e.now(), onboarded: true, isDM: true, skills: skills,
		hostTools: c.HasScope(character.ScopeCharacters)})
	resp, err := e.LLM.Chat(llm.WithMeta(ctx, llm.Meta{Character: c.ID, Purpose: "voice-check", Origin: llm.OriginTask}), llm.Request{
		Model:     c.Model,
		Messages:  []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: prompt}},
		MaxTokens: 1200,
	})
	if err != nil {
		return "", err
	}
	return cleanReply(resp.Message.Content, c), nil
}

const voiceJudgeRubric = `You check whether a character's new reply still sounds like its reference reply. Judge voice, not facts: the two can say different things.
- tone: same personality and warmth/energy as the reference and the character sheet.
- length: roughly the same size (within about half to double the reference's words) and the same shape (no lists or headings where the reference has none).
- catchphrases: catchphrases and signature words used about as sparingly as in the reference (none is fine; one in every sentence is not).
- in_role: speaks as the character, never as a generic assistant or AI model, and breaks none of the "never says" rules.
Reply with JSON only: {"tone": true|false, "length": true|false, "catchphrases": true|false, "in_role": true|false, "note": "one short sentence on the biggest difference"}`

// JudgeVoice compares a replayed reply with the reference using the cheap
// model. Never-say phrases are checked in code too.
func (e *Engine) JudgeVoice(ctx context.Context, c *character.Character, vt character.VoiceTest, reply string) (VoiceVerdict, error) {
	temp := 0.0
	sheet := fmt.Sprintf("Name: %s\nRole: %s\nTraits: %s\nCatchphrases: %s\nVoice rules:\n- %s\nNever says: %s",
		c.DisplayName(), c.Role, strings.Join(c.Personality.Traits, ", "), strings.Join(c.Personality.Catchphrases, " | "),
		strings.Join(c.Voice.Style, "\n- "), strings.Join(c.Voice.NeverSay, " | "))
	msg := fmt.Sprintf("Character:\n%s\n\nThe user said:\n%s\n\nReference reply (%d words):\n%s\n\nNew reply (%d words):\n%s",
		sheet, vt.Prompt, len(strings.Fields(vt.Reference)), vt.Reference, len(strings.Fields(reply)), reply)
	if vt.Notes != "" {
		msg += "\n\nWhat matters in this exchange: " + vt.Notes
	}
	resp, err := e.LLM.Chat(llm.WithMeta(ctx, llm.Meta{Character: c.ID, Purpose: "voice-check", Origin: llm.OriginTask}), llm.Request{
		Model:       e.Settings.String(ctx, "cheap_model"),
		Messages:    []llm.Message{{Role: "system", Content: voiceJudgeRubric}, {Role: "user", Content: msg}},
		Temperature: &temp, MaxTokens: 200, JSON: true, NoFallback: true,
	})
	if err != nil {
		return VoiceVerdict{}, err
	}
	var raw struct {
		Tone         *bool  `json:"tone"`
		Length       *bool  `json:"length"`
		Catchphrases *bool  `json:"catchphrases"`
		InRole       *bool  `json:"in_role"`
		Note         string `json:"note"`
	}
	s := strings.TrimSpace(resp.Message.Content)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```"))
	if err := json.Unmarshal([]byte(s), &raw); err != nil || raw.Tone == nil || raw.Length == nil || raw.Catchphrases == nil || raw.InRole == nil {
		return VoiceVerdict{}, fmt.Errorf("unreadable verdict from the judge: %q", truncate(s, 200))
	}
	v := VoiceVerdict{Tone: *raw.Tone, Length: *raw.Length, Catchphrases: *raw.Catchphrases, InRole: *raw.InRole, Note: raw.Note}
	if banned := bannedPhrases(c, reply); len(banned) > 0 {
		v.InRole = false
		v.Note = strings.TrimSpace(strings.Join(banned, "; ") + ". " + v.Note)
	}
	return v, nil
}
