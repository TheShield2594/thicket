package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Approvals are answered in chat ("approve"/"deny") or from the dashboard.
// Either way decide runs the action and the sprig reports back in the chat
// the request was made in. Approvals whose tool starts with "thicket:" are
// actions Thicket itself performs (create a sprig, raise a level), not
// tools the model may call.

const internalPrefix = "thicket:"

// handleApprovalReply resolves a pending action from a chat reply.
func (e *Engine) handleApprovalReply(ctx context.Context, in channel.Incoming, user *store.User) (bool, error) {
	approve, deny := approveRe.MatchString(in.Text), denyRe.MatchString(in.Text)
	if !approve && !deny {
		return false, nil
	}
	a, err := e.DB.LatestPendingApproval(ctx, user.ID, in.ChatRef(), approvalMaxAge)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if in.NoApprovals {
		host, _ := e.Chars.Host()
		return true, e.send(ctx, in.ChatRef(), host, "Approvals can't be answered from here: an agent connected to Thicket sent this, and only you can approve. "+
			"Approve or deny it on the dashboard (the \"needs you\" list) or in your chat app.")
	}
	ok, err := e.decide(ctx, user, a, approve, in.Text, in.IsDM)
	if !ok {
		return false, err
	}
	return true, err
}

// ErrNotYours is returned when someone answers another person's request.
var ErrNotYours = errors.New("that request isn't yours to answer")

// DecideApproval answers a pending approval from outside the chat (the
// dashboard). Only the person who was asked can answer.
func (e *Engine) DecideApproval(ctx context.Context, user *store.User, id int64, approve bool) error {
	a, err := e.DB.ApprovalByID(ctx, id)
	if err != nil {
		return err
	}
	if a.UserID != user.ID {
		return ErrNotYours
	}
	if a.Status != "pending" || e.now().Sub(a.CreatedAt) > approvalMaxAge {
		return errors.New("that request was already answered or has expired")
	}
	unlock := e.lockChat(a.ChatRef) // in turn with the chat
	defer unlock()
	word := "approved"
	if !approve {
		word = "denied"
	}
	ok, err := e.decide(ctx, user, a, approve, "", true)
	if err == nil && !ok {
		err = errors.New("that request was already answered")
	}
	if err == nil {
		e.logActivity(ctx, "info", "approval", a.Character, user.ID, fmt.Sprintf("approval #%d %s from the dashboard", a.ID, word), "")
	}
	return err
}

// decide records the answer, runs the action if approved, and lets the
// sprig report. said is what the user typed ("" from the dashboard). It
// returns false if the approval was no longer pending. Everything that can
// fail before the action runs happens before the approval is claimed, so a
// failure leaves it pending for another try.
func (e *Engine) decide(ctx context.Context, user *store.User, a *store.Approval, approve bool, said string, isDM bool) (bool, error) {
	char, ok := e.Chars.Get(a.Character)
	gone := !ok
	if gone {
		// The sprig that asked was removed: never act under another
		// sprig's permissions. The host only reports it.
		if char, ok = e.Chars.Host(); !ok {
			return false, errors.New("no host character is loaded")
		}
	}
	conv, err := e.DB.OpenConversation(ctx, user.ID, a.ChatRef, e.Cfg.IdleAfter)
	if err != nil {
		return false, err
	}
	status := "approved"
	switch {
	case !approve:
		status = "denied"
	case gone:
		status = "expired"
	}
	if ok, err := e.DB.DecideApproval(ctx, a.ID, status); err != nil || !ok {
		return false, err
	}
	e.doneWaiting(a.Character)
	if said != "" {
		_ = e.DB.AddMessage(ctx, conv.ID, "user", "", said)
	} else {
		_ = e.DB.AddMessage(ctx, conv.ID, "event", "", "The user answered a request from the dashboard.")
	}
	detail := e.Secrets.Redact(a.Args)
	if a.Tool == internalPrefix+"create_sprig" && privateProposal(a.Args) {
		detail = "(a private sprig's files: not logged)" // admins read the activity log
	}
	if a.Tool == internalPrefix+"import_sprig" {
		detail = "(a sprig package)"
	}
	e.logActivity(ctx, "info", "approval", a.Character, user.ID, fmt.Sprintf("approval #%d %s (%s)", a.ID, status, a.Tool), detail)
	t := &turn{user: user, char: char, chatRef: a.ChatRef, conv: conv, isDM: isDM, approvedEstimate: a.EstimateID}
	what := strings.TrimPrefix(a.Tool, internalPrefix)
	switch {
	case !approve:
		return true, e.respond(ctx, t, fmt.Sprintf("The user said no to %s. Acknowledge briefly and don't try it again unless they ask.", what))
	case gone:
		// Reported in code, not by a model turn: nothing here may run a
		// tool or hand off.
		_ = e.DB.SetApprovalResult(ctx, a.ID, "not run: the sprig that asked no longer exists")
		msg := fmt.Sprintf("I didn't run %s: %s, who asked for it, isn't around any more.", what, a.Character)
		if err := e.send(ctx, a.ChatRef, char, msg); err != nil {
			return true, err
		}
		_ = e.DB.AddMessage(ctx, conv.ID, "assistant", char.ID, msg)
		return true, nil
	}
	var result string
	if strings.HasPrefix(a.Tool, internalPrefix) {
		out, err := e.internalAction(ctx, user, a)
		if err != nil {
			out = "Error: " + e.Secrets.Redact(err.Error())
			e.logActivity(ctx, "warn", "approval", char.ID, user.ID, "approved "+what+" failed", e.Secrets.Redact(err.Error()))
		}
		result = out
	} else {
		result = e.runApproved(ctx, t, a.Tool, json.RawMessage(a.Args), false)
	}
	_ = e.DB.SetApprovalResult(ctx, a.ID, truncate(result, 2000))
	return true, e.respond(ctx, t, fmt.Sprintf("The user approved %s. Result:\n%s\nTell them how it went, honestly.", what, result))
}

// runApproved runs a tool a person said yes to. The level in force now
// still applies: an approval whose sprig was lowered to read or draft
// since it asked doesn't run. A draft (fromDraft) may run at draft level,
// since the person read it and said yes, but not at read.
func (e *Engine) runApproved(ctx context.Context, t *turn, name string, args json.RawMessage, fromDraft bool) string {
	tool := e.toolsFor(ctx, t)[name]
	if tool == nil {
		return "Error: that tool is no longer available to you."
	}
	if err := e.paused(ctx, t.char.ID); err != nil {
		return "Error: " + err.Error() + ", so it wasn't run."
	}
	// A draft was admitted by RunDraft before it was claimed, so a limit
	// can't leave it resolved but not done.
	if !fromDraft {
		if msg := e.rateLimited(ctx, t, tool, false); msg != "" {
			return msg
		}
	}
	if changes(tool) {
		if lv := e.toolLevel(ctx, t.char, tool); lv == store.LevelRead || (lv == store.LevelDraft && !fromDraft) {
			return "Error: your access to " + name + " was lowered to " + lv + " since you asked, so it wasn't run."
		}
	}
	// Approved means not asked again, but never past a cap that has
	// closed in since it was asked.
	if msg := e.approvedCost(ctx, t, tool, args); msg != "" {
		return msg
	}
	runCtx, finish := ctx, func() {}
	if id := t.toolEstimate; id != 0 && tool.Name != "delegate_task" { // a background task is measured where it runs
		var m *budget.Meter
		runCtx, m = budget.WithMeter(ctx)
		finish = func() { _ = e.DB.FinishEstimate(context.WithoutCancel(ctx), id, m.USD()) }
	}
	out, err := tool.Run(runCtx, t, args)
	finish()
	raw := out
	if err != nil {
		// Integration errors can quote credentials: redact before logging.
		out = "Error: " + e.Secrets.Redact(err.Error())
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "approved tool "+name+" failed", out)
	} else {
		e.logActivity(ctx, "info", "tool", t.char.ID, t.user.ID, "ran approved tool "+name, e.Secrets.Redact(truncate(out, 500)))
	}
	// Redact before anything else sees it: the model, the DB, the chat.
	out = e.Secrets.Redact(out)
	if err == nil {
		e.recordAction(ctx, t, tool, args, raw)
	}
	if tool.Untrusted {
		return wrapUntrusted(name, out)
	}
	return out
}

// RunDraft has a drafted action done, from the dashboard: the person
// read the exact call and said yes. It refuses if the sprig is now at
// read level for that tool.
func (e *Engine) RunDraft(ctx context.Context, user *store.User, id int64) error {
	it, err := e.DB.InboxItemByID(ctx, id)
	if err != nil {
		return err
	}
	if it.UserID != user.ID {
		return ErrNotYours
	}
	if it.Kind != store.InboxDraft || it.Status != "open" || it.Action == "" {
		return errors.New("that isn't an open draft")
	}
	var act draftAction
	if err := json.Unmarshal([]byte(it.Action), &act); err != nil {
		return err
	}
	char, ok := e.Chars.Get(it.Character)
	if !ok {
		return fmt.Errorf("the sprig %q no longer exists", it.Character)
	}
	chatRef := act.ChatRef
	if chatRef == "" {
		if chatRef, err = e.DB.LastChatRef(ctx, user.ID); err != nil || chatRef == "" {
			return errors.New("there's no chat to report the result in")
		}
	}
	unlock := e.lockChat(chatRef)
	defer unlock()
	// Checks first, claim last: a draft stays open unless it's really run.
	conv, err := e.DB.OpenConversation(ctx, user.ID, chatRef, e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	t := &turn{user: user, char: char, chatRef: chatRef, conv: conv, isDM: true}
	tool := e.toolsFor(ctx, t)[act.Tool]
	if tool == nil {
		return errors.New(char.DisplayName() + " can't use " + act.Tool + " any more")
	}
	if e.toolLevel(ctx, char, tool) == store.LevelRead {
		return errors.New(char.DisplayName() + " is at read level for that tool now, so it can't be done")
	}
	if err := e.paused(ctx, char.ID); err != nil {
		return fmt.Errorf("%w: resume it first", err)
	}
	// Rate limits before the claim: a draft over a limit stays open.
	if msg := e.rateLimited(ctx, t, tool, false); msg != "" {
		return errors.New(strings.TrimPrefix(msg, "Error: "))
	}
	if err := e.DB.ResolveInbox(ctx, it.ID, user.ID); err != nil {
		return errors.New("that draft was already dealt with")
	}
	e.logActivity(ctx, "info", "draft", char.ID, user.ID, fmt.Sprintf("draft #%d approved from the dashboard", it.ID), "")
	result := e.runApproved(ctx, t, act.Tool, act.Args, true)
	_ = e.DB.AddMessage(ctx, conv.ID, "event", "", "The user had your draft of "+act.Tool+" done from the dashboard.")
	return e.respond(ctx, t, fmt.Sprintf("The user read your draft of %s and had it done. Result:\n%s\nTell them how it went, briefly.", act.Tool, result))
}

// internalAction performs an approved "thicket:" action.
func (e *Engine) internalAction(ctx context.Context, user *store.User, a *store.Approval) (string, error) {
	switch a.Tool {
	case internalPrefix + "create_sprig":
		return e.createSprig(ctx, user, json.RawMessage(a.Args))
	case internalPrefix + "raise_level":
		return e.raiseLevel(ctx, user, json.RawMessage(a.Args))
	case internalPrefix + "import_sprig":
		return e.importSprig(ctx, user, json.RawMessage(a.Args))
	}
	return "", fmt.Errorf("unknown action %s", a.Tool)
}

// askAs queues an approval prompt posted by another sprig than the one
// whose turn it is (Juni presents trust requests).
func (t *turn) askAs(c *character.Character, text string, a *store.Approval) {
	t.afterReply = append(t.afterReply, post{text: text, approval: a, as: c})
}
