package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

// Show your work. Every action a sprig takes that changes something (an
// integration's write tool, scheduling a job) goes in the trail with one
// plain sentence written by Thicket, never by the model: what was done,
// with which tool, on what. In a chat the sentence is posted right after
// the reply, with how to undo it:
//
//   - "undo" in reply to that line (or just "undo", for the last one in
//     this chat) reverses it, handled in code like "pin this";
//   - an integration declares the call that reverses each tool (its undo
//     map: delete what was created); a tool with none is recorded as not
//     undoable, and the sentence says so instead of offering undo. At ask
//     level the approval prompt says it before the action runs.
//
// A sprig whose character file sets actions.quiet keeps routine built-in
// work (scheduling, its webhooks and triggers) to the trail; integration
// actions are always said. Read-only work is never posted.

// undoWindow is how long a bare "undo" reaches back.
const undoWindow = 24 * time.Hour

var undoRe = regexp.MustCompile(`(?i)^\s*(?:please\s+)?undo(?:\s+(?:that|this|it))?(?:[\s,]+please)?[\s.!]*$`)

// routineTools are built-in tools that change something; they say what
// they did in their own words.
var routineTools = map[string]bool{"schedule_job": true, "create_webhook": true, "create_trigger": true, "update_job": true}

var jobIDRe = regexp.MustCompile(`(?i)\bjob #(\d+)`)

// action is what a tool call did, said in plain words.
type action struct {
	said    string     // one sentence, no undo hint
	undo    *trailUndo // nil: can't be undone from here
	label   string     // what undo does ("remove it")
	routine bool
}

// inverseFor is the declared undo of an integration's tool, if any.
func (e *Engine) inverseFor(ctx context.Context, server, tool string) *store.InverseCall {
	servers, err := e.DB.MCPServers(ctx)
	if err != nil {
		return nil
	}
	for _, s := range servers {
		if s.Name == server {
			if inv, ok := s.Undo[tool]; ok && inv.Tool != "" {
				return &inv
			}
		}
	}
	return nil
}

// undoNote says, before an action at ask level, whether it can be undone.
func (e *Engine) undoNote(ctx context.Context, tool *Tool) string {
	if !changes(tool) {
		return ""
	}
	if server, ok := strings.CutPrefix(tool.Scope, "mcp:"); ok {
		if e.inverseFor(ctx, server, tool.Remote) != nil {
			return "↩️ You can undo this afterwards."
		}
		return "⚠️ This can't be undone once it's done."
	}
	if tool.Name == "schedule_job" || tool.Name == "create_webhook" {
		return "↩️ You can undo this afterwards."
	}
	return ""
}

// describe turns a finished tool call into an action.
func (e *Engine) describe(ctx context.Context, t *turn, tool *Tool, args json.RawMessage, out string) action {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	if server, ok := strings.CutPrefix(tool.Scope, "mcp:"); ok {
		act := action{said: fmt.Sprintf("I used %s's %s%s.", server, tool.Remote, argSummary(a))}
		inv := e.inverseFor(ctx, server, tool.Remote)
		if inv == nil {
			return act
		}
		var result map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &result)
		if inv.Said != "" {
			if s, ok := fill(inv.Said, a, result, out); ok {
				act.said = strings.TrimRight(s, ". ") + "."
			}
		}
		undoArgs := map[string]any{}
		for k, v := range inv.Args {
			val, ok := fillValue(v, a, result, out)
			if !ok {
				return act // the undo needs something the result didn't give
			}
			undoArgs[k] = val
		}
		raw, _ := json.Marshal(undoArgs)
		act.undo = &trailUndo{Type: "mcp", Character: t.char.ID, Server: server, Tool: inv.Tool, Args: raw}
		act.label = inv.Label
		if act.label == "" {
			act.label = "undo it"
		}
		return act
	}
	name, _ := a["name"].(string)
	act := action{routine: true}
	switch tool.Name {
	case "schedule_job":
		act.said = fmt.Sprintf("I scheduled %q.", name)
	case "create_webhook":
		act.said = fmt.Sprintf("I set up the webhook %q.", name)
	case "create_trigger":
		act.said = fmt.Sprintf("I set up the trigger %q.", name)
	case "update_job":
		what, _ := a["action"].(string)
		act.said = fmt.Sprintf("I changed job #%v (%s).", a["job_id"], what)
	default:
		act.said = fmt.Sprintf("I used %s%s.", tool.Name, argSummary(a))
	}
	if tool.Name == "schedule_job" || tool.Name == "create_webhook" {
		if m := jobIDRe.FindStringSubmatch(out); m != nil {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			act.undo, act.label = &trailUndo{Type: "job", Character: t.char.ID, EventID: id}, "delete it"
		}
	}
	return act
}

// argSummary is " (key: value, …)" for a call's short arguments.
func argSummary(a map[string]any) string {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var v string
		switch x := a[k].(type) {
		case string:
			v = x
		case float64, bool:
			v = fmt.Sprint(x)
		default:
			continue
		}
		if v == "" || len(v) > 60 || strings.ContainsAny(v, "\n") {
			continue
		}
		parts = append(parts, k+": "+v)
		if len(parts) == 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

var placeholderRe = regexp.MustCompile(`\{\{\s*(args|result)(?:\.([A-Za-z0-9_.-]+))?\s*\}\}`)

// lookup finds a placeholder's value.
func lookup(kind, path string, args, result map[string]any, out string) (any, bool) {
	if kind == "result" && path == "" {
		return strings.TrimSpace(out), strings.TrimSpace(out) != ""
	}
	var cur any = args
	if kind == "result" {
		cur = result
	}
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok || cur == nil {
			return nil, false
		}
	}
	return cur, true
}

// fill renders a template as text. ok is false if a value is missing.
func fill(tmpl string, args, result map[string]any, out string) (string, bool) {
	ok := true
	s := placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		v, found := lookup(sub[1], sub[2], args, result, out)
		if !found {
			ok = false
			return ""
		}
		if s, isStr := v.(string); isStr {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	})
	return s, ok
}

// fillValue renders one argument: a lone placeholder keeps its value's
// type (an id stays a number).
func fillValue(tmpl string, args, result map[string]any, out string) (any, bool) {
	if m := placeholderRe.FindStringSubmatch(strings.TrimSpace(tmpl)); m != nil && m[0] == strings.TrimSpace(tmpl) {
		return lookup(m[1], m[2], args, result, out)
	}
	return fill(tmpl, args, result, out)
}

// recordAction puts a finished act-level action in the trail and, in a
// chat, queues the sentence saying it (with how to undo it) after the
// reply.
func (e *Engine) recordAction(ctx context.Context, t *turn, tool *Tool, args json.RawMessage, out string) {
	if !changes(tool) || tool.Trailed || t.consult != nil {
		return
	}
	act := e.describe(ctx, t, tool, args, out)
	detail := e.Secrets.Redact(truncate(string(args), 500))
	id := e.addTrail(ctx, t.user.ID, t.char.ID, store.TrailAction, act.said, detail, act.undo)
	if t.chatRef == "" || id == 0 || act.routine && t.char.Actions.Quiet {
		return
	}
	text := act.said
	switch {
	case act.undo != nil:
		text += " Reply **undo** to " + act.label + "."
	case !act.routine:
		text += " This can't be undone from here."
	}
	t.afterReply = append(t.afterReply, post{text: text, trailID: id})
}

// undoCommand handles "undo": in reply to the line that said what a sprig
// did, or on its own for the newest such line in this chat. handled is
// false when there's nothing to undo here (the sprig answers instead).
func (e *Engine) undoCommand(ctx context.Context, in channel.Incoming, user *store.User) (ack string, handled bool, err error) {
	var id int64
	if in.ReplyTo != "" {
		tid, owner, err := e.DB.TrailAck(ctx, in.ChatRef(), in.ReplyTo)
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", true, err
		}
		if owner != user.ID {
			return "That was done for someone else, so it's theirs to undo.", true, nil
		}
		id = tid
	} else {
		tid, err := e.DB.LatestTrailAck(ctx, in.ChatRef(), user.ID, e.now().Add(-undoWindow))
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", true, err
		}
		id = tid
	}
	msg, err := e.UndoTrail(ctx, user, id)
	if errors.Is(err, ErrAlreadyUndone) {
		return "That's already been undone.", true, nil
	}
	if err != nil {
		return "I couldn't undo that: " + shortErr(err), true, nil
	}
	return msg, true, nil
}

// undoAction reverses an integration action or a scheduled job.
func (e *Engine) undoAction(ctx context.Context, by *store.User, u trailUndo) (string, error) {
	switch u.Type {
	case "job":
		j, err := e.DB.JobByID(ctx, u.EventID)
		if err != nil {
			return "", fmt.Errorf("job #%d is already gone", u.EventID)
		}
		if err := e.DB.DeleteJob(ctx, j.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("job #%d %q is deleted", j.ID, j.Name), nil
	case "mcp":
		c, ok := e.Chars.Get(u.Character)
		if !ok {
			return "", fmt.Errorf("the sprig %q no longer exists", u.Character)
		}
		if err := e.paused(ctx, c.ID); err != nil {
			return "", fmt.Errorf("%w: resume it first", err)
		}
		res, err := e.MCP.CallAs(ctx, c.ID, u.Server, u.Tool, u.Args, false, e.execAllowed(ctx, c))
		if err != nil {
			return "", fmt.Errorf("%s's %s failed: %s", u.Server, u.Tool, shortErr(err))
		}
		if res.IsError {
			return "", fmt.Errorf("%s's %s failed: %s", u.Server, u.Tool, truncate(e.Secrets.Redact(res.Text), 200))
		}
		e.logActivity(ctx, "info", "tool", c.ID, by.ID, "undid an action with "+u.Server+"'s "+u.Tool, e.Secrets.Redact(truncate(string(u.Args), 300)))
		return fmt.Sprintf("I called %s's %s%s", u.Server, u.Tool, argSummary(argsMap(u.Args))), nil
	}
	return "", fmt.Errorf("don't know how to undo %q", u.Type)
}

func argsMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
