package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Who may change what a sprig is allowed to do (permission levels, tool
// grants, exec, paths):
//
//   - a solo sprig: only the person it belongs to. Not admins, not the
//     owner: it's private.
//   - a family sprig (shared by the household): the owner and admins.
//   - a kid sprig: the owner and admins; it is locked to read whatever
//     they set, and never gets exec or paths.
//
// Members can't change family sprigs or anyone else's. A refused attempt
// is logged and goes in the trail, so the household can see it.

// canManage reports whether by may change sprig c's permissions (c nil:
// every sprig's), without logging anything.
func canManage(by *store.User, c *character.Character) bool {
	switch {
	case by == nil || by.IsKid():
		return false
	case c == nil:
		return by.IsAdmin()
	case c.Tier() == character.TierSolo:
		return c.BelongsTo == by.ID
	}
	return by.IsAdmin()
}

// mayManage reports whether by may change sprig c's permissions, and logs
// a refusal. what says what was tried, for the log ("raise a level").
func (e *Engine) mayManage(ctx context.Context, by *store.User, c *character.Character, what string) error {
	if by == nil {
		return errors.New("no one to check permissions for")
	}
	if c == nil {
		if by.IsAdmin() {
			return nil
		}
		e.refused(ctx, by, "", "tried to "+what+" for every sprig")
		return errors.New("only the owner or an admin can change what every sprig may do")
	}
	switch {
	case by.IsKid():
	case c.Tier() == character.TierSolo:
		if c.BelongsTo == by.ID {
			return nil
		}
		// Someone else's private sprig: don't even confirm it exists.
		e.refused(ctx, by, c.ID, "tried to "+what+" on someone else's private sprig")
		return fmt.Errorf("there's no sprig called %q", c.ID)
	case by.IsAdmin():
		return nil
	}
	e.refused(ctx, by, c.ID, fmt.Sprintf("tried to %s on %s, which isn't theirs", what, c.DisplayName()))
	if c.IsKid() {
		return fmt.Errorf("only the owner or an admin can change what %s may do", c.DisplayName())
	}
	return fmt.Errorf("%s is shared by the household, so only the owner or an admin can change what it may do", c.DisplayName())
}

// refused records an attempt to change a sprig that wasn't the person's
// to change: in the activity log and the trail (no undo).
func (e *Engine) refused(ctx context.Context, by *store.User, char, summary string) {
	who := "someone"
	if by != nil && by.Name != "" {
		who = by.Name
	} else if by != nil {
		who = fmt.Sprintf("user %d", by.ID)
	}
	e.logActivity(ctx, "warn", "approval", char, userID(by), "refused: "+who+" "+summary, "")
	e.addTrail(ctx, userID(by), char, store.TrailRefused, who+" "+summary, "", nil)
}

// ownsSolo reports whether id is userID's own solo sprig.
func (e *Engine) ownsSolo(id string, userID int64) bool {
	c, ok := e.Chars.Get(id)
	return ok && c.Tier() == character.TierSolo && c.BelongsTo == userID
}
