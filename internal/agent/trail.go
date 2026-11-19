package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Every permission change and every sprig-created event goes in the trail
// with what undoes it. Undo is one call (UndoTrail), from the dashboard,
// the API or `thicket trail undo`.

// trailUndo is the undo payload. Type decides which fields matter.
type trailUndo struct {
	Type      string   `json:"type"` // level | grant | allow | event | role | cap (Tool = period, Level = the cap to restore)
	Character string   `json:"character,omitempty"`
	Tool      string   `json:"tool,omitempty"`
	Level     string   `json:"level,omitempty"` // the level to restore ("" = none set)
	Tools     []string `json:"tools,omitempty"` // the scopes to restore
	EventID   int64    `json:"event_id,omitempty"`
	UserID    int64    `json:"user_id,omitempty"`
	Role      string   `json:"role,omitempty"`
}

func (e *Engine) addTrail(ctx context.Context, userID int64, char, kind, summary, detail string, undo *trailUndo) int64 {
	var raw json.RawMessage
	if undo != nil {
		raw, _ = json.Marshal(undo)
	}
	id, err := e.DB.AddTrailID(ctx, store.TrailEntry{UserID: userID, Character: char, Kind: kind, Summary: summary, Detail: detail, Undo: raw})
	if err != nil {
		e.Log.Error("trail", "err", err)
	}
	return id
}

// setLevel changes a stored level and records it in the trail.
func (e *Engine) setLevel(ctx context.Context, by *store.User, who, tool, level, why string) error {
	if c, ok := e.Chars.Get(who); ok && c.IsKid() && level != "" && level != store.LevelRead {
		return fmt.Errorf("%s is a kid sprig: it's locked to read", c.DisplayName())
	}
	prev, err := e.DB.ExactToolLevel(ctx, who, tool)
	if err != nil {
		return err
	}
	if err := e.DB.SetToolLevel(ctx, who, tool, level); err != nil {
		return err
	}
	e.trailLevel(ctx, by, who, tool, prev, level, why)
	return nil
}

func (e *Engine) trailLevel(ctx context.Context, by *store.User, who, tool, prev, level, why string) {
	show := func(l string) string {
		if l == "" {
			return "default"
		}
		return l
	}
	label := who
	if who == store.AllSprigs {
		label = "every sprig"
	}
	var uid int64
	if by != nil {
		uid = by.ID
	}
	e.addTrail(ctx, uid, strings.TrimPrefix(who, store.AllSprigs), store.TrailLevel,
		fmt.Sprintf("%s for %s: %s → %s", tool, label, show(prev), show(level)), why,
		&trailUndo{Type: "level", Character: who, Tool: tool, Level: prev})
}

// trailGrant records a change of a sprig's tool scopes.
func (e *Engine) trailGrant(ctx context.Context, by *store.User, c *character.Character, prev, next []string) {
	var uid int64
	if by != nil {
		uid = by.ID
	}
	e.addTrail(ctx, uid, c.ID, store.TrailGrant, fmt.Sprintf("%s's tools: %s → %s", c.DisplayName(), listOr(prev), listOr(next)), "",
		&trailUndo{Type: "grant", Character: c.ID, Tools: prev})
}

func listOr(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// ErrAlreadyUndone means the entry was undone before (or can't be).
var ErrAlreadyUndone = errors.New("that was already undone, or there's nothing to undo")

// UndoTrail reverses one trail entry. Only admins undo permission changes;
// a person may undo events a sprig put on their own calendar.
func (e *Engine) UndoTrail(ctx context.Context, by *store.User, id int64) (string, error) {
	t, err := e.DB.TrailByID(ctx, id)
	if err != nil {
		return "", err
	}
	if !t.Undoable {
		return "", ErrAlreadyUndone
	}
	var u trailUndo
	if err := json.Unmarshal(t.Undo, &u); err != nil {
		return "", fmt.Errorf("unreadable undo for trail entry %d: %w", id, err)
	}
	if by.IsKid() {
		return "", errors.New("kids can't undo changes")
	}
	parentOf := func() bool {
		c, ok := e.Chars.Get(u.Character)
		return ok && c.IsParent(by.ID)
	}
	switch {
	case (u.Type == "level" || u.Type == "grant" || u.Type == "allow" || u.Type == "exec" || u.Type == "path") && u.Character != store.AllSprigs && e.Chars.PrivateFrom(u.Character, by.ID):
		// someone else's private sprig: theirs to change, not undo
		return "", fmt.Errorf("no sprig %q", u.Character)
	case u.Type == "cap":
		// Through SetSprigCap's own rules: only a kid sprig's parents
		// raise its cap, whoever undoes.
		if e.Chars.PrivateFrom(u.Character, by.ID) {
			return "", fmt.Errorf("no sprig %q", u.Character)
		}
	case u.Type == "event", u.Type == "restore_event", by.IsAdmin():
	case (u.Type == "level" || u.Type == "exec" || u.Type == "path") && u.Character != store.AllSprigs && e.ownsSolo(u.Character, by.ID):
		// a member undoing a change to their own private sprig
	case (u.Type == "grant" || u.Type == "allow") && parentOf():
		// a parent undoing a change to their kid's sprig
	default:
		return "", errors.New("only the owner or an admin can undo permission changes")
	}
	// Claim the undo first, so two at once can't both run it.
	if ok, err := e.DB.MarkTrailUndone(ctx, id, by.ID); err != nil || !ok {
		return "", ErrAlreadyUndone
	}
	done, err := e.undo(ctx, by, u)
	if err != nil {
		_ = e.DB.ClearTrailUndone(ctx, id)
		return "", err
	}
	e.addTrail(ctx, by.ID, t.Character, store.TrailUndo, fmt.Sprintf("undid #%d: %s", id, t.Summary), done, nil)
	e.logActivity(ctx, "info", "approval", t.Character, by.ID, fmt.Sprintf("undid trail #%d", id), done)
	return "Undone: " + done + ".", nil
}

func (e *Engine) undo(ctx context.Context, by *store.User, u trailUndo) (string, error) {
	var done string
	switch u.Type {
	case "level":
		if err := e.DB.SetToolLevel(ctx, u.Character, u.Tool, u.Level); err != nil {
			return "", err
		}
		done = fmt.Sprintf("%s for %s is back to %s", u.Tool, u.Character, orDefault(u.Level))
	case "grant":
		c, ok := e.Chars.Get(u.Character)
		if !ok {
			return "", fmt.Errorf("the sprig %q no longer exists", u.Character)
		}
		cp := *c
		cp.Tools = slices.Clone(u.Tools)
		sort.Strings(cp.Tools)
		if _, err := e.Chars.Write(ctx, &cp); err != nil {
			return "", err
		}
		done = fmt.Sprintf("%s's tools are back to %s", c.DisplayName(), listOr(cp.Tools))
	case "allow":
		c, ok := e.Chars.Get(u.Character)
		if !ok {
			return "", fmt.Errorf("the sprig %q no longer exists", u.Character)
		}
		cp := *c
		cp.Allow = slices.Clone(u.Tools)
		if _, err := e.Chars.Write(ctx, &cp); err != nil {
			return "", err
		}
		done = fmt.Sprintf("%s may see %s again", c.DisplayName(), listOr(cp.Allow))
	case "event":
		ev, err := e.DB.SprigEventByID(ctx, u.EventID)
		if err != nil {
			return "", err
		}
		if !by.IsAdmin() && ev.UserID != by.ID {
			return "", errors.New("that event isn't yours to undo")
		}
		if err := e.DB.DeleteSprigEvent(ctx, u.EventID); err != nil {
			return "", err
		}
		done = fmt.Sprintf("%q is off the calendar", ev.Title)
	case "restore_event":
		ev, err := e.DB.SprigEventAny(ctx, u.EventID)
		if err != nil {
			return "", err
		}
		if !by.IsAdmin() && !ev.RemovableBy(by) {
			return "", errors.New("that event isn't yours to put back")
		}
		if err := e.DB.RestoreSprigEvent(ctx, u.EventID); err != nil {
			return "", err
		}
		done = fmt.Sprintf("%q is back on the calendar", ev.Title)
	case "remove_user":
		if err := e.DB.DeleteFreshUser(ctx, u.UserID); err != nil {
			return "", err
		}
		done = fmt.Sprintf("the invite for user %d is withdrawn", u.UserID)
	case "exec", "path":
		return e.undoAccess(ctx, by, u)
	case "cap":
		usd, _ := strconv.ParseFloat(u.Level, 64)
		s, err := e.SetSprigCap(ctx, by, u.Character, u.Tool, usd)
		if err != nil {
			return "", err
		}
		done = strings.TrimSuffix(s, ".")
	case "role":
		if err := e.DB.SetUserRole(ctx, u.UserID, u.Role); err != nil {
			return "", err
		}
		done = fmt.Sprintf("user %d is back to %s", u.UserID, u.Role)
	default:
		return "", fmt.Errorf("don't know how to undo %q", u.Type)
	}
	return done, nil
}

func orDefault(l string) string {
	if l == "" {
		return "default"
	}
	return l
}

// SetToolLevel changes a sprig's level for a tool on someone's behalf
// (the API): only by the sprig's owner (see mayManage), recorded in the
// trail, never above read for a kid sprig. level "" clears it.
func (e *Engine) SetToolLevel(ctx context.Context, by *store.User, sprig, tool, level string) error {
	if level != "" && !store.ValidLevel(level) {
		return errors.New("level must be read, draft, ask, act or empty")
	}
	var target *character.Character
	if sprig != store.AllSprigs {
		c, ok := e.Chars.Get(sprig)
		if !ok {
			return fmt.Errorf("no sprig %q", sprig)
		}
		target = c // someone else's private sprig: mayManage says it doesn't exist
	}
	if err := e.mayManage(ctx, by, target, "change a permission level"); err != nil {
		return err
	}
	return e.setLevel(ctx, by, sprig, tool, level, "set through the API")
}
