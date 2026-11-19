package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// Trust upgrades: a sprig asks for a higher level on one tool. Juni puts
// the request to the user with the sprig's track record from the trail
// (actions taken, approval rate, mistakes); the level changes only if an
// owner or admin approves.

const trackRecordDays = 30

type levelRequest struct {
	Sprig string `json:"sprig"`
	Tool  string `json:"tool"`
	Level string `json:"level"`
	From  string `json:"from"`
}

func (e *Engine) toolRequestAccess(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Tool   string `json:"tool"`
		Level  string `json:"level"`
		Reason string `json:"reason"`
	}](args)
	if err != nil {
		return "", err
	}
	tool := e.toolsFor(ctx, t)[a.Tool]
	if tool == nil || tool.Name == "request_access" {
		return "", fmt.Errorf("you don't have a tool called %q; you can only ask for more trust with tools you already have", a.Tool)
	}
	cur := e.toolLevel(ctx, t.char, tool)
	if store.LevelRank(a.Level) <= store.LevelRank(cur) {
		return "", fmt.Errorf("you're already at %s for %s; you can only ask for a higher level (read < draft < ask < act)", cur, a.Tool)
	}
	if strings.TrimSpace(a.Reason) == "" {
		return "", errors.New("say why you need it: the user will read your reason")
	}
	if !t.user.IsAdmin() {
		return "", errors.New("only the owner or an admin can change what sprigs may do; tell the user what you'd need")
	}
	host, ok := e.Chars.Host()
	if !ok {
		return "", errors.New("no host to present the request")
	}
	if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("trust:%s:%s", t.char.ID, a.Tool), time.Hour); !ok {
		return "", errors.New("you asked for this within the last hour; wait for the user's answer")
	}
	rec, err := e.DB.ToolRecordSince(ctx, t.char.ID, a.Tool, e.now().AddDate(0, 0, -trackRecordDays))
	if err != nil {
		return "", err
	}
	chatRef := t.chatRef
	if chatRef == "" {
		if chatRef, err = e.DB.LastChatRef(ctx, t.user.ID); err != nil || chatRef == "" {
			return "", errors.New("there's no chat to ask the user in")
		}
	}
	b, _ := json.Marshal(levelRequest{Sprig: t.char.ID, Tool: a.Tool, Level: a.Level, From: cur})
	ap := &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: chatRef, Tool: internalPrefix + "raise_level", Args: string(b)}
	msg := fmt.Sprintf("🌿 %s is asking for more trust with **%s**: %s → **%s**.\nWhy: %s\n\n%s\n\nReply **approve** or **deny**.",
		t.char.DisplayName(), tool.Label(), cur, a.Level, strings.TrimSpace(a.Reason), describeRecord(rec))
	if t.chatRef != "" {
		t.askAs(host, msg, ap)
	} else if err := e.deliverApproval(ctx, host, ap, msg); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "approval", t.char.ID, t.user.ID, fmt.Sprintf("asked to raise %s to %s", a.Tool, a.Level), a.Reason)
	return fmt.Sprintf("PENDING: %s has put your request to the user with your track record. Tell them in a sentence why you asked; don't assume the answer.", host.DisplayName()), nil
}

// describeRecord is the track record as the user reads it.
func describeRecord(r store.ToolRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Track record, last %d days:\n", trackRecordDays)
	fmt.Fprintf(&b, "- actions taken: %d", r.Actions)
	if r.Drafts > 0 {
		fmt.Fprintf(&b, ", drafts written: %d", r.Drafts)
	}
	b.WriteString("\n")
	if asked := r.Approved + r.Denied; asked > 0 {
		fmt.Fprintf(&b, "- approvals: %d of %d approved (%d%%)", r.Approved, asked, 100*r.Approved/asked)
		if r.Expired > 0 {
			fmt.Fprintf(&b, ", %d left unanswered", r.Expired)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- approvals: none asked yet\n")
	}
	mistakes := []string{}
	if r.Failures > 0 {
		mistakes = append(mistakes, fmt.Sprintf("%d failed call(s)", r.Failures))
	}
	if r.Denied > 0 {
		mistakes = append(mistakes, fmt.Sprintf("%d request(s) you turned down", r.Denied))
	}
	if r.Refused > 0 {
		mistakes = append(mistakes, fmt.Sprintf("%d attempt(s) beyond its access", r.Refused))
	}
	if len(mistakes) == 0 {
		b.WriteString("- mistakes: none")
	} else {
		b.WriteString("- mistakes: " + strings.Join(mistakes, ", "))
	}
	if r.Actions+r.Drafts+r.Approved+r.Denied == 0 {
		b.WriteString("\n(No history with this tool yet.)")
	}
	return b.String()
}

// raiseLevel applies an approved trust upgrade.
func (e *Engine) raiseLevel(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	if !user.IsAdmin() {
		return "", errors.New("only the owner or an admin can change levels")
	}
	var r levelRequest
	if err := json.Unmarshal(args, &r); err != nil {
		return "", err
	}
	c, ok := e.Chars.Get(r.Sprig)
	if !ok {
		return "", fmt.Errorf("the sprig %q no longer exists", r.Sprig)
	}
	if c.IsKid() {
		return "", fmt.Errorf("%s is a kid sprig: it's locked to read", c.DisplayName())
	}
	prevExact, _ := e.DB.ExactToolLevel(ctx, c.ID, r.Tool)
	// The level may have changed since the request (lowered, say): only
	// raise from the level the user was shown. Checked and written in one
	// transaction; a failed read refuses.
	tool := e.toolsFor(ctx, &turn{user: user, char: c})[r.Tool]
	if tool == nil {
		return "", fmt.Errorf("%s no longer has %s", c.DisplayName(), r.Tool)
	}
	def := store.LevelAct
	if tool.Risky {
		def = store.LevelAsk
	}
	cur, err := e.DB.RaiseToolLevel(ctx, c.ID, r.Tool, granted(tool), def, r.From, r.Level)
	if errors.Is(err, store.ErrLevelChanged) {
		return "", fmt.Errorf("%s's level for %s changed to %s since it asked (was %s), so nothing was changed; it can ask again", c.DisplayName(), r.Tool, cur, r.From)
	}
	if err != nil {
		return "", fmt.Errorf("couldn't check %s's current level, so nothing was changed: %w", c.DisplayName(), err)
	}
	e.trailLevel(ctx, user, c.ID, r.Tool, prevExact, r.Level, "approved trust request")
	e.logActivity(ctx, "info", "approval", c.ID, user.ID, fmt.Sprintf("%s raised to %s for %s", r.Tool, r.Level, c.DisplayName()), "was "+r.From)
	return fmt.Sprintf("%s is now at %s for %s (was %s).", c.DisplayName(), r.Level, r.Tool, r.From), nil
}
