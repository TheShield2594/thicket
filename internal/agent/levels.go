package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Permission levels decide, per sprig and tool, what happens when a sprig
// calls a tool that changes something (Tool.Risky):
//
//	read  - refused: the sprig may only use tools that read
//	draft - not run: the exact action goes to the user's inbox as a draft
//	ask   - run after the user approves (the default for risky tools)
//	act   - run straight away (the default for everything else)
//
// Tools that only read run at every level except ask, which always asks.
// Built-in tools that change something without needing approval by
// default (scheduling a job) are Tool.Writes: they act by default, but
// read and draft hold them back like risky ones.
//
// Levels are set per sprig and tool, per sprig for all its granted tools
// (tool "*": new sprigs start at draft), or per tool for all sprigs; the
// most specific wins. Core tools (memory, teamwork) ignore a sprig-wide
// level: a sprig at draft still remembers things.

// changes reports whether a tool changes something outside the chat.
func changes(t *Tool) bool { return t.Risky || t.Writes }

// granted reports whether a tool comes from a scope the sprig was given,
// so a sprig-wide level applies to it.
func granted(t *Tool) bool { return t.Scope != "" && t.Scope != "host" }

// toolLevel returns the level that applies to this sprig and tool.
func (e *Engine) toolLevel(ctx context.Context, c *character.Character, tool *Tool) string {
	if c.IsKid() {
		return store.LevelRead // whatever is stored: kid sprigs never change anything
	}
	lv, err := e.DB.ToolLevel(ctx, c.ID, tool.Name, granted(tool))
	if err != nil {
		e.Log.Error("read tool level; asking instead", "tool", tool.Name, "err", err)
		return store.LevelAsk // fail closed
	}
	if lv != "" {
		return lv
	}
	if tool.Risky {
		return store.LevelAsk
	}
	return store.LevelAct
}

// gate applies the level to a call. It returns ok=true when the tool may
// run now; otherwise msg is what the model is told instead.
func (e *Engine) gate(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) (msg string, ok bool) {
	switch lv := e.toolLevel(ctx, t.char, tool); {
	case lv == store.LevelAct && e.holdTainted(ctx, t, tool, args):
		return e.holdAction(ctx, t, tool, args), false
	case lv == store.LevelAct:
		return "", true
	case lv == store.LevelAsk:
		return e.requestApproval(ctx, t, tool, args), false
	case !changes(tool):
		return "", true // read and draft both allow reading
	case lv == store.LevelRead:
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "refused "+tool.Name+": read-only access", "")
		return fmt.Sprintf("Error: you only have read access to %s, so you can't use it to change anything. Tell the user what you would have done; they can raise your access.", tool.Label()), false
	default: // draft
		return e.draftAction(ctx, t, tool, args), false
	}
}

// draftAction writes the action up for the user instead of running it.
func (e *Engine) draftAction(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) string {
	act, _ := json.Marshal(draftAction{Tool: tool.Name, Args: args, ChatRef: t.chatRef})
	// The draft goes to the sprig's owner: a private sprig's person, or
	// whoever a shared sprig was working for.
	_, err := e.DB.AddInbox(ctx, store.InboxItem{UserID: heldOwner(t), Character: t.char.ID, Kind: store.InboxDraft,
		Title: fmt.Sprintf("%s drafted: %s", t.char.DisplayName(), tool.Label()), Detail: prettyArgs(args), Action: string(act)})
	if err != nil {
		e.Log.Error("save draft", "err", err)
		return "Error: couldn't save the draft: " + err.Error()
	}
	e.logActivity(ctx, "info", "draft", t.char.ID, t.user.ID, "drafted "+tool.Name, e.Secrets.Redact(truncate(string(args), 500)))
	return "DRAFTED: you're at draft level for this tool, so it was NOT done. The exact action is saved in the user's inbox for them to review. Tell them briefly what you drafted; don't say it's done."
}

// draftAction is what a draft would have done, kept so the user can have
// it done later (from the dashboard).
type draftAction struct {
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	ChatRef string          `json:"chat_ref,omitempty"`
}

// prettyArgs indents the arguments without re-encoding them, so what a
// person reads is exactly what runs (no number rounding).
func prettyArgs(args json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, args, "", "  ") == nil {
		return b.String()
	}
	return string(args)
}

func (e *Engine) toolSetLevel(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Tool  string `json:"tool"`
		Level string `json:"level"`
		Sprig string `json:"sprig"`
	}](args)
	if err != nil {
		return "", err
	}
	tool := strings.TrimSpace(a.Tool)
	if tool == "" {
		return "", errors.New("tool is required")
	}
	who, label := store.AllSprigs, "every sprig"
	var target *character.Character
	if s := strings.TrimSpace(a.Sprig); s != "" && s != store.AllSprigs {
		c, err := e.findSprig(nil, s)
		if err != nil {
			return "", err
		}
		who, label, target = c.ID, c.DisplayName(), c
	}
	// Admins set levels on shared sprigs (and for every sprig); a private
	// sprig's levels are its owner's alone. Refusals are logged.
	if err := e.mayManage(ctx, t.user, target, "change a permission level"); err != nil {
		return "", err
	}
	if tool == store.AnyTool && who == store.AllSprigs {
		return "", errors.New(`tool "*" needs a sprig: name the sprig whose granted tools it covers`)
	}
	level := a.Level
	if level == "default" {
		level = ""
	} else if !store.ValidLevel(level) {
		return "", errors.New("level must be read, draft, ask, act or default")
	}
	if err := e.setLevel(ctx, t.user, who, tool, level, "set in chat"); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "approval", t.char.ID, t.user.ID, fmt.Sprintf("%s level for %s set to %s", tool, label, a.Level), "")
	return fmt.Sprintf("%s is now at %s for %s.", tool, a.Level, label), nil
}
