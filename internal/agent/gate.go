package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// The proactive gate. Every message a sprig starts itself (job and
// trigger output, check-ins, follow-ups) passes holdReason: inside the
// sprig's quiet hours, or once its daily budget for that person is used
// up, it waits. Replies to the person never pass here and never count.
//
// Finished output (a job's result) is held in the database, so a restart
// doesn't lose it, and FlushHeld delivers it once nothing holds it back
// any more. Check-ins and follow-ups aren't written until they can go out:
// what they're about stays due, so they're simply tried again later.
//
// A message held for the budget waits for the next day's budget (the
// person's local midnight), like one held for quiet hours waits for the
// morning. Either way a held message older than heldMaxAge is stale: it
// goes to the inbox instead of the chat, and the activity log says so.
//
// Things that must get through (approvals, safety alerts to parents,
// failures that need the person) don't pass the gate and don't count;
// mustReach notes why when the gate would have held them.

// heldMaxAge is how long a held message stays worth delivering.
const heldMaxAge = 24 * time.Hour

// heldClaimTimeout is how long a flush may hold a message it is
// delivering before another flush may take it over (after a crash).
const heldClaimTimeout = 10 * time.Minute

// holdReason says why a message c starts itself for u must wait now:
// store.HeldQuiet, store.HeldBudget, or "" when it may go out.
func (e *Engine) holdReason(ctx context.Context, c *character.Character, u *store.User) string {
	if e.quiet(c, u) {
		return store.HeldQuiet
	}
	if e.sentToday(ctx, c, u) >= c.DailyBudget() {
		return store.HeldBudget
	}
	return ""
}

// sentToday counts the proactive messages c sent u today, in u's timezone.
func (e *Engine) sentToday(ctx context.Context, c *character.Character, u *store.User) int {
	n, err := e.DB.ProactiveSentSince(ctx, u.ID, c.ID, store.LocalDayStart(e.now(), u.Location()))
	if err != nil {
		e.Log.Warn("count proactive messages", "err", err)
		return 0 // fail open: a database hiccup shouldn't silence a sprig
	}
	return n
}

// sendProactive delivers a proactive message and counts it in the budget.
func (e *Engine) sendProactive(ctx context.Context, u *store.User, c *character.Character, text string) error {
	if err := e.sendToUser(ctx, u, c, text); err != nil {
		return err
	}
	if err := e.DB.RecordProactive(context.WithoutCancel(ctx), u.ID, c.ID, e.now()); err != nil {
		e.Log.Error("count proactive message", "err", err)
	}
	return nil
}

// deliverProactive sends finished output a sprig started itself, or holds
// it for later. held reports which; an error means it was neither sent
// nor held, and the caller keeps it somewhere else (the inbox).
func (e *Engine) deliverProactive(ctx context.Context, u *store.User, c *character.Character, text, ref, title string) (held bool, err error) {
	why := e.holdReason(ctx, c, u)
	if why == "" {
		return false, e.sendProactive(ctx, u, c, text)
	}
	if _, err := e.DB.HoldMessage(context.WithoutCancel(ctx), store.HeldMessage{UserID: u.ID, Character: c.ID, Ref: ref, Title: title,
		Reason: why, Text: text, CreatedAt: e.now()}); err != nil {
		return false, fmt.Errorf("hold the message: %w", err)
	}
	e.logActivity(ctx, "info", "initiative", c.ID, u.ID, fmt.Sprintf("held %s: %s", title, holdLabel(why, c)), "")
	return true, nil
}

func holdLabel(why string, c *character.Character) string {
	if why == store.HeldBudget {
		return fmt.Sprintf("today's %d proactive messages were already sent", c.DailyBudget())
	}
	return "quiet hours (" + c.Initiative.QuietHours + ")"
}

// FlushHeld delivers held messages nothing holds back any more, oldest
// first, and moves stale ones to the inbox. The initiative loop runs it.
func (e *Engine) FlushHeld(ctx context.Context) {
	held, err := e.DB.HeldMessages(ctx, 0, "")
	if err != nil {
		e.Log.Error("list held messages", "err", err)
		return
	}
	for _, h := range held {
		user, err := e.DB.UserByID(ctx, h.UserID)
		if err != nil {
			continue // deleted users take their held messages with them
		}
		char, ok := e.Chars.Get(h.Character)
		if !ok {
			if char, ok = e.Chars.Host(); !ok {
				continue
			}
		}
		stale := e.now().Sub(h.CreatedAt) > heldMaxAge
		if !stale {
			if why := e.holdReason(ctx, char, user); why != "" {
				if why != h.Reason {
					_ = e.DB.SetHeldReason(ctx, h.ID, why)
				}
				continue
			}
		}
		// Claimed, not removed: the message stays stored until it has
		// reached the chat or the inbox, so a crash in between can't lose
		// it (at worst it's sent twice).
		if claimed, err := e.DB.ClaimHeld(ctx, h.ID, e.now(), e.now().Add(-heldClaimTimeout)); err != nil || !claimed {
			continue
		}
		if stale {
			err := e.deadLetterErr(ctx, store.InboxItem{UserID: user.ID, Character: char.ID, Kind: store.InboxUndelivered, Ref: h.Ref,
				Title: fmt.Sprintf("%s was held too long to send (%s)", h.Title, holdLabel(h.Reason, char)), Detail: h.Text})
			if err != nil {
				_ = e.DB.ReleaseHeld(ctx, h.ID)
				continue
			}
			e.finishHeld(ctx, h.ID)
			e.logActivity(ctx, "warn", "initiative", char.ID, user.ID, fmt.Sprintf("dropped held %s: it was more than a day old; it's in the inbox", h.Title), "")
			continue
		}
		text := h.Text + "\n" + heldNote(h, user)
		if err := e.sendProactive(ctx, user, char, text); err != nil {
			e.Log.Error("deliver held message", "id", h.ID, "err", err)
			// Not sent: keep it held (released for the next flush) unless the
			// inbox takes it.
			if e.deadLetterErr(ctx, store.InboxItem{UserID: user.ID, Character: char.ID, Kind: store.InboxUndelivered, Ref: h.Ref,
				Title: fmt.Sprintf("%s was held, but couldn't be delivered", h.Title), Detail: h.Text}) != nil {
				_ = e.DB.ReleaseHeld(ctx, h.ID)
				continue
			}
			e.finishHeld(ctx, h.ID)
			continue
		}
		e.finishHeld(ctx, h.ID)
		e.logActivity(ctx, "info", "initiative", char.ID, user.ID, "delivered held "+h.Title, "")
	}
	if err := e.DB.PruneProactive(ctx, e.now().Add(-72*time.Hour)); err != nil {
		e.Log.Warn("prune proactive counts", "err", err)
	}
}

// finishHeld removes a delivered message. If that fails it stays claimed,
// and is tried again once the claim goes stale.
func (e *Engine) finishHeld(ctx context.Context, id int64) {
	if err := e.DB.FinishHeld(context.WithoutCancel(ctx), id); err != nil {
		e.Log.Error("remove delivered held message", "id", id, "err", err)
	}
}

// heldNote tells the person when a held message was written and why it
// waited.
func heldNote(h store.HeldMessage, u *store.User) string {
	at := h.CreatedAt.In(u.Location()).Format("Mon 15:04")
	if h.Reason == store.HeldBudget {
		return fmt.Sprintf("_(From %s: held because I'd used up the day's messages.)_", at)
	}
	return fmt.Sprintf("_(From %s: held until your quiet hours ended.)_", at)
}

// mustReach is for messages that bypass quiet hours and the budget
// because they must get through: it returns a line saying why when the
// gate would have held the message ("" otherwise), and logs it. Such
// messages aren't counted in the budget.
func (e *Engine) mustReach(ctx context.Context, c *character.Character, u *store.User, why string) string {
	if c == nil || u == nil {
		return ""
	}
	when := ""
	switch e.holdReason(ctx, c, u) {
	case store.HeldQuiet:
		when = "during your quiet hours"
	case store.HeldBudget:
		when = "past today's message limit"
	default:
		return ""
	}
	e.logActivity(ctx, "info", "initiative", c.ID, u.ID, "sent "+when+": "+why, "")
	return fmt.Sprintf("\n_(Sent %s because %s.)_", when, why)
}
