package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"

	"gopkg.in/yaml.v3"
)

// New sprigs: Juni interviews the user (job, personality, voice, tools,
// schedule), previews the sprig in its own voice, then create_character
// shows every file the sprig will get as a diff and asks for approval.
// Only the approved proposal is written, exactly as shown; the sprig
// starts with all its granted tools at draft.

// sprigDraft is the latest preview, per user.
type sprigDraft struct {
	char     *character.Character
	look     avatar.Params
	routines []routineArg
}

// sprigProposal is what the user approves: the files, byte for byte.
type sprigProposal struct {
	ID       string            `json:"id"`
	Files    map[string]string `json:"files"` // path relative to the characters dir -> content
	Routines []routineArg      `json:"routines,omitempty"`
}

// voiceTestPrompts start a new sprig's voice tests; the references are
// recorded later with `thicket voice-check ID --record`.
var voiceTestPrompts = []struct{ name, prompt, notes string }{
	{"01-hello", "hi! how's it going?", "A plain hello: their usual length and warmth."},
	{"02-what-do-you-do", "remind me what you help me with?", "Their job, in their own voice, not a feature list."},
	{"03-something-broke", "something you did for me earlier didn't work. what happened?", "Owns the problem plainly, stays in character."},
}

func (e *Engine) buildProposal(d *sprigDraft) (*sprigProposal, error) {
	md, err := character.Marshal(d.char)
	if err != nil {
		return nil, err
	}
	id := d.char.ID
	p := &sprigProposal{ID: id, Files: map[string]string{id + ".md": string(md)}, Routines: d.routines}
	p.Files[id+"/avatar.json"] = string(avatar.Marshal(d.look))
	for _, vt := range voiceTestPrompts {
		b, err := yaml.Marshal(character.VoiceTest{Prompt: vt.prompt, Notes: vt.notes})
		if err != nil {
			return nil, err
		}
		p.Files[id+"/voice_tests/"+vt.name+".yaml"] = string(b)
	}
	return p, nil
}

// sortedFiles lists a proposal's paths: the character file first.
func (p *sprigProposal) sortedFiles() []string {
	out := []string{p.ID + ".md"}
	var rest []string
	for path := range p.Files {
		if path != p.ID+".md" {
			rest = append(rest, path)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// newFileDiff is a unified diff that creates path with content.
func newFileDiff(path, content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/characters/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// diffMessage is what the user sees before approving.
func (p *sprigProposal) diffMessage(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🌱 Here's everything %s will get. Nothing is live until you say so.\n```diff\n", name)
	for _, path := range p.sortedFiles() {
		b.WriteString(newFileDiff(path, p.Files[path]))
	}
	b.WriteString("```\n")
	if len(p.Routines) > 0 {
		b.WriteString("Routines (run in your timezone):\n")
		for _, r := range p.Routines {
			fmt.Fprintf(&b, "- **%s** `%s`: %s\n", r.Name, r.Schedule, r.Task)
		}
	}
	fmt.Fprintf(&b, "%s starts with every tool at **draft**: actions are written up for you, not done, until you give more trust.\nReply **approve** or **deny**.", name)
	return b.String()
}

func (e *Engine) toolCreateCharacter(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		ID string `json:"id"`
	}](args)
	if err != nil {
		return "", err
	}
	v, ok := e.drafts.Load(t.user.ID)
	if !ok {
		return "", errors.New("there's no preview to create; call preview_character first so the user can see the sprig")
	}
	d := v.(*sprigDraft)
	if d.char.ID != strings.ToLower(a.ID) {
		return "", fmt.Errorf("the latest preview is %q, not %q", d.char.ID, a.ID)
	}
	if _, exists := e.Chars.Get(d.char.ID); exists {
		return "", fmt.Errorf("a sprig with id %q already exists", d.char.ID)
	}
	p, err := e.buildProposal(d)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(p)
	ap := &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef, Tool: internalPrefix + "create_sprig", Args: string(b)}
	t.askAs(t.char, p.diffMessage(d.char.DisplayName()), ap)
	e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, "proposed new sprig "+d.char.DisplayName(), "")
	return fmt.Sprintf("PENDING: the user is being shown every file %s will get, as a diff, and asked to approve. Tell them briefly to look it over; don't say it's created.", d.char.DisplayName()), nil
}

// createSprig writes an approved proposal, exactly as shown.
func (e *Engine) createSprig(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	var p sprigProposal
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	c, err := character.Parse([]byte(p.Files[p.ID+".md"]))
	if err != nil {
		return "", err
	}
	if c.ID != p.ID {
		return "", errors.New("the proposal doesn't match its character file")
	}
	if _, exists := e.Chars.Get(c.ID); exists {
		return "", fmt.Errorf("a sprig with id %q was created meanwhile", c.ID)
	}
	for _, s := range c.Tools {
		if s == character.ScopeAdmin || s == character.ScopeCharacters {
			return "", errors.New("new sprigs can't be given the " + s + " scope from chat")
		}
		if strings.HasPrefix(s, character.ScopeMCPPrefix) && !user.IsAdmin() {
			return "", errors.New("only the owner or an admin can give a sprig integration access")
		}
	}
	// The folder first: once the .md exists, the sprig is live.
	dir := e.Chars.Dir()
	for path, content := range p.Files {
		if path == p.ID+".md" {
			continue
		}
		clean := filepath.Clean(path)
		if !strings.HasPrefix(clean, p.ID+string(filepath.Separator)) || strings.Contains(clean, "..") {
			return "", fmt.Errorf("refusing to write %q", path)
		}
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	// Every granted tool starts at draft, before the sprig can act.
	if err := e.DB.SetToolLevel(ctx, c.ID, store.AnyTool, store.LevelDraft); err != nil {
		return "", err
	}
	path, err := e.Chars.Write(ctx, c)
	if err != nil {
		return "", err
	}
	var made []string
	for _, r := range p.Routines {
		if _, err := scheduler.Parse(r.Schedule); err != nil {
			return "", fmt.Errorf("routine %q: %w", r.Name, err)
		}
		j, err := e.DB.CreateJob(ctx, store.Job{UserID: user.ID, Character: c.ID, Name: r.Name, Schedule: r.Schedule, Prompt: r.Task, Kind: store.JobCron})
		if err != nil {
			return "", err
		}
		made = append(made, fmt.Sprintf("#%d %s", j.ID, r.Name))
	}
	e.drafts.Delete(user.ID)
	e.logActivity(ctx, "info", "character", c.ID, user.ID, fmt.Sprintf("new sprig %s sprouted", c.DisplayName()), path)
	out := fmt.Sprintf("%s has been created (%s), with every tool at draft. They can now be addressed by name, e.g. \"%s, ...\".", c.DisplayName(), path, c.DisplayName())
	if len(made) > 0 {
		out += " Routines scheduled: " + strings.Join(made, ", ") + "."
	}
	return out, nil
}
