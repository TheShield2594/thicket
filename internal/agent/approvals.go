package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// returns false if the approval was no longer pending.
func (e *Engine) decide(ctx context.Context, user *store.User, a *store.Approval, approve bool, said string, isDM bool) (bool, error) {
	ans, err := e.claim(ctx, user, a, approve, isDM)
	if ans == nil {
		return false, err
	}
	// Answered without its buttons: take them off the prompt.
	e.settlePrompt(ctx, a, ans.outcome())
	if said == "" {
		ans.note = "The user answered a request from the dashboard."
	}
	return true, e.carryOut(ctx, ans, said)
}

// answer is an approval claimed with a yes or no, waiting to be carried
// out.
type answer struct {
	a       *store.Approval
	t       *turn
	approve bool
	gone    bool   // the sprig that asked no longer exists
	note    string // event recorded when the user typed nothing
}

// outcome is what the prompt's message shows once it's answered.
func (ans *answer) outcome() string {
	switch {
	case ans.gone:
		return "Not run: the sprig that asked is gone"
	case ans.approve:
		return "✅ Approved"
	}
	return "✖️ Denied"
}

// claim moves an approval out of pending. Everything that can fail
// before the action runs happens before it is claimed, so a failure
// leaves it pending for another try. A nil answer means it was no longer
// pending (or an error).
func (e *Engine) claim(ctx context.Context, user *store.User, a *store.Approval, approve, isDM bool) (*answer, error) {
	char, ok := e.Chars.Get(a.Character)
	gone := !ok
	if gone {
		// The sprig that asked was removed: never act under another
		// sprig's permissions. The host only reports it.
		if char, ok = e.Chars.Host(); !ok {
			return nil, errors.New("no host character is loaded")
		}
	}
	conv, err := e.DB.OpenConversation(ctx, user.ID, a.ChatRef, e.Cfg.IdleAfter)
	if err != nil {
		return nil, err
	}
	status := "approved"
	switch {
	case !approve:
		status = "denied"
	case gone:
		status = "expired"
	}
	if ok, err := e.DB.DecideApproval(ctx, a.ID, status); err != nil || !ok {
		return nil, err
	}
	e.doneWaiting(a.Character)
	detail := e.Secrets.Redact(a.Args)
	if a.Tool == internalPrefix+"create_sprig" && privateProposal(a.Args) {
		detail = "(a private sprig's files: not logged)" // admins read the activity log
	}
	if a.Tool == internalPrefix+"import_sprig" {
		detail = "(a sprig package)"
	}
	if a.Tool == internalPrefix+"add_starter" && strings.Contains(a.Args, `"tier":"solo"`) {
		detail = "(a private sprig: not logged)" // admins read the activity log
	}
	e.logActivity(ctx, "info", "approval", a.Character, user.ID, fmt.Sprintf("approval #%d %s (%s)", a.ID, status, a.Tool), detail)
	t := &turn{user: user, char: char, chatRef: a.ChatRef, conv: conv, isDM: isDM, approvedEstimate: a.EstimateID}
	return &answer{a: a, t: t, approve: approve, gone: gone}, nil
}

// carryOut runs a claimed approval's action if it was approved and lets
// the sprig report. said is what the user typed, if anything.
func (e *Engine) carryOut(ctx context.Context, ans *answer, said string) error {
	a, t, approve, gone := ans.a, ans.t, ans.approve, ans.gone
	user, char, conv := t.user, t.char, t.conv
	if said != "" {
		_ = e.DB.AddMessage(ctx, conv.ID, "user", "", said)
	} else if ans.note != "" {
		_ = e.DB.AddMessage(ctx, conv.ID, "event", "", ans.note)
	}
	what := strings.TrimPrefix(a.Tool, internalPrefix)
	switch {
	case !approve:
		return e.respond(ctx, t, fmt.Sprintf("The user said no to %s. Acknowledge briefly and don't try it again unless they ask.", what))
	case gone:
		// Reported in code, not by a model turn: nothing here may run a
		// tool or hand off.
		_ = e.DB.SetApprovalResult(ctx, a.ID, "not run: the sprig that asked no longer exists")
		msg := fmt.Sprintf("I didn't run %s: %s, who asked for it, isn't around any more.", what, a.Character)
		if err := e.send(ctx, a.ChatRef, char, msg); err != nil {
			return err
		}
		_ = e.DB.AddMessage(ctx, conv.ID, "assistant", char.ID, msg)
		return nil
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
	return e.respond(ctx, t, fmt.Sprintf("The user approved %s. Result:\n%s\nTell them how it went, honestly.", what, result))
}

// approvalAction prefixes the ids of an approval prompt's buttons:
// "approval:<id>:approve" and "approval:<id>:deny".
const approvalAction = "approval:"

// approvalActions are the buttons under an approval prompt, labelled as
// in the web app. Typing approve or deny still works everywhere.
func approvalActions(id int64) []channel.Action {
	return []channel.Action{
		{ID: fmt.Sprintf("%s%d:approve", approvalAction, id), Label: "Approve", Style: channel.ActionPrimary},
		{ID: fmt.Sprintf("%s%d:deny", approvalAction, id), Label: "Deny", Style: channel.ActionDanger},
	}
}

// HandlePress answers a tap on a message's button in a chat app. It
// returns quickly: slow work (running an approved action, the sprig's
// reply) carries on after it.
func (e *Engine) HandlePress(ctx context.Context, p channel.Press) (r channel.PressReply) {
	defer func() {
		if v := recover(); v != nil {
			e.Log.Error("panic handling a button press", "panic", v)
			r = channel.PressReply{Notice: "Something went wrong on my end. Try again, or reply instead."}
		}
	}()
	if rest, ok := strings.CutPrefix(p.ActionID, approvalAction); ok {
		return e.pressApproval(ctx, p, rest)
	}
	return channel.PressReply{Notice: "That button doesn't do anything any more."}
}

// pressApproval answers an Approve or Deny button. Only the person the
// approval belongs to can press it, in the chat it was asked in; anyone
// else is told privately. Presses come from people in a chat app, never
// from an agent (the local API and MCP server have no buttons).
func (e *Engine) pressApproval(ctx context.Context, p channel.Press, rest string) channel.PressReply {
	idText, verb, _ := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || (verb != "approve" && verb != "deny") {
		return channel.PressReply{Notice: "That button doesn't do anything any more."}
	}
	notYours := channel.PressReply{Notice: "This isn't yours to approve."}
	oops := channel.PressReply{Notice: "Something went wrong on my end. Try again, or reply approve or deny."}
	user, err := e.DB.UserByExternal(ctx, p.Channel, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return notYours
	}
	if err != nil {
		e.Log.Error("button press: look up user", "err", err)
		return oops
	}
	a, err := e.DB.ApprovalByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return channel.PressReply{Notice: "That request no longer exists."}
	}
	if err != nil {
		e.Log.Error("button press: look up approval", "err", err)
		return oops
	}
	if a.UserID != user.ID || a.ChatRef != p.ChatRef() {
		return notYours
	}
	if a.Reserved {
		// Tapped in the moment between the prompt going out and it being
		// opened: nothing is settled, so the next tap works.
		return channel.PressReply{Notice: "One moment: that request is still being set up. Tap again."}
	}
	if a.Status != "pending" || e.now().Sub(a.CreatedAt) > approvalMaxAge {
		// This press settles the message, so nothing else has to.
		_, _ = e.DB.TakePromptMsg(ctx, a.ID)
		return channel.PressReply{Notice: "That request was already answered or has expired.", Outcome: settledLabel(a, e.now())}
	}
	// Claim it now, so the message settles at once; the action and the
	// sprig's reply follow in turn with the chat.
	approve := verb == "approve"
	ans, err := e.claim(ctx, user, a, approve, p.IsDM)
	if err != nil {
		e.Log.Error("button press: decide approval", "err", err)
		return oops
	}
	if ans == nil {
		return channel.PressReply{Notice: "That request was already answered."}
	}
	// The channel settles the pressed message with the reply.
	_, _ = e.DB.TakePromptMsg(ctx, a.ID)
	word, outcome := "denied", ans.outcome()
	if approve {
		word = "approved"
	}
	ans.note = fmt.Sprintf("The user %s the request with a button.", word)
	e.logActivity(ctx, "info", "approval", a.Character, user.ID, fmt.Sprintf("approval #%d %s with a button", a.ID, word), "")
	go func() {
		defer func() {
			if v := recover(); v != nil {
				e.Log.Error("panic carrying out an approval", "panic", v)
			}
		}()
		unlock := e.lockChat(a.ChatRef)
		defer unlock()
		if err := e.carryOut(ctx, ans, ""); err != nil {
			e.Log.Error("carry out approval", "err", err, "approval", a.ID)
		}
	}()
	return channel.PressReply{Outcome: outcome}
}

// settlePrompt takes the buttons off an approval's prompt and shows how
// it ended, for an approval answered without them (typed, or from the
// dashboard). It doesn't wait for the chat app, and a failure is only
// logged: the answer stands either way.
func (e *Engine) settlePrompt(ctx context.Context, a *store.Approval, outcome string) {
	msg, err := e.DB.TakePromptMsg(ctx, a.ID)
	if err != nil {
		e.Log.Warn("settle approval prompt", "err", err, "approval", a.ID)
		return
	}
	if msg == "" {
		return
	}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				e.Log.Error("panic settling an approval prompt", "panic", v)
			}
		}()
		e.settleMsg(context.WithoutCancel(ctx), a, msg, outcome)
	}()
}

// settleMsg edits the message with an approval's buttons to show outcome
// without them, on channels that can.
func (e *Engine) settleMsg(ctx context.Context, a *store.Approval, msg, outcome string) {
	name, chatID := splitRef(a.ChatRef)
	s, ok := e.channelFor(name).(channel.ActionSettler)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	if err := s.SettleActions(ctx, chatID, msg, outcome); err != nil {
		e.Log.Warn("couldn't take the buttons off an approval prompt", "err", err, "approval", a.ID, "channel", name)
	}
}

// settleTimeout bounds one edit of a prompt; settleEvery is how often
// expired approvals' prompts are settled.
const (
	settleTimeout = 30 * time.Second
	settleEvery   = time.Minute
)

// SettleApprovals expires approvals nobody answered in time and settles
// the prompts of those that ended with their buttons still showing:
// expired ones, and any whose edit was missed.
func (e *Engine) SettleApprovals(ctx context.Context) {
	aps, err := e.DB.UnsettledApprovals(ctx, approvalMaxAge)
	if err != nil {
		e.Log.Warn("unsettled approvals", "err", err)
		return
	}
	for i := range aps {
		a := &aps[i]
		msg, err := e.DB.TakePromptMsg(ctx, a.ID)
		if err != nil || msg == "" {
			continue
		}
		e.settleMsg(ctx, a, msg, settledLabel(a, e.now()))
	}
}

// RunApprovalSweep settles expired approvals' prompts until ctx ends.
func (e *Engine) RunApprovalSweep(ctx context.Context) {
	t := time.NewTicker(settleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.SettleApprovals(ctx)
		}
	}
}

// settledLabel says how an approval that's no longer waiting ended.
func settledLabel(a *store.Approval, now time.Time) string {
	switch {
	case a.Status == "approved":
		return "✅ Approved"
	case a.Status == "denied":
		return "✖️ Denied"
	case a.Status == "pending" && now.Sub(a.CreatedAt) <= approvalMaxAge:
		return ""
	}
	return "Expired"
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
		lv := e.toolLevel(ctx, t.char, tool)
		if f, ok := e.floorFor(ctx, t, tool); ok {
			lv = stricter(lv, f.Level) // a floor set since it asked counts too
		}
		if lv == store.LevelRead || (lv == store.LevelDraft && !fromDraft) {
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
	case internalPrefix + "approve_skill":
		return e.approveSkill(ctx, user, json.RawMessage(a.Args))
	case internalPrefix + "add_starter":
		return e.addStarter(ctx, user, json.RawMessage(a.Args))
	case internalPrefix + "create_kid_sprig":
		return e.createKidSprigApproved(ctx, user, json.RawMessage(a.Args))
	}
	return "", fmt.Errorf("unknown action %s", a.Tool)
}

// askAs queues an approval prompt posted by another sprig than the one
// whose turn it is (Juni presents trust requests).
func (t *turn) askAs(c *character.Character, text string, a *store.Approval) {
	t.afterReply = append(t.afterReply, post{text: text, approval: a, as: c})
}
