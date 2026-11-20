package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/store"
)

// Level floors. Some calls must ask first whatever level a sprig's tool
// is at: a browser on a payment page, or the first runs of a sprig's own
// skill. A floor is a minimum level for a call; the level that applies is
// the stricter of the configured one and the floor (levels.go), so a
// floor of ask turns act into ask but leaves draft and read as they are.
//
// Floors come from two places:
//
//   - Core: while a sprig follows one of its own skills for one of the
//     first skillFloorRuns times (a run with ForcedAsk), every call that
//     changes something asks.
//   - A plugin: a tool result's _meta "thicket/floor" ({"min_level":
//     "ask", "reason": "payment page"}) floors the integration's next
//     calls in that chat, for floorTTL. A plugin can raise a floor, never
//     lower one: a level looser than ask counts as ask, a later result
//     can't remove it, and it ends only when it expires.
//
// Nothing the model says (tool arguments) or a tool's output text can
// set or remove a floor. Floors apply only to calls that change
// something (changes()), and never loosen a kid sprig (always read).

const (
	// skillFloorRuns is how many runs of a sprig's own skill ask first.
	skillFloorRuns = 3
	// floorTTL is how long a plugin's floor lasts after it last set it.
	floorTTL = 30 * time.Minute
	// skillRunFloorAge bounds how long an unfinished forced run keeps
	// flooring its chat (a run the sprig never finished).
	skillRunFloorAge = 6 * time.Hour
	maxFloorReason   = 80
)

// Floor is a minimum level for a call, with why, for the approval prompt
// and the trail.
type Floor struct {
	Level  string
	Reason string
}

// strictness orders levels: lower is stricter.
func strictness(level string) int {
	switch level {
	case store.LevelRead:
		return 0
	case store.LevelDraft:
		return 1
	case store.LevelAsk:
		return 2
	default: // act, or anything unknown, is the loosest
		return 3
	}
}

// stricter returns the stricter of two levels.
func stricter(a, b string) string {
	if strictness(b) < strictness(a) {
		return b
	}
	return a
}

// pluginFloor turns a plugin's floor metadata into a floor of at least
// ask: a missing, unknown or looser level counts as ask.
func pluginFloor(server string, m *mcp.FloorMeta) Floor {
	lv := strings.ToLower(strings.TrimSpace(m.MinLevel))
	if !store.ValidLevel(lv) || strictness(lv) > strictness(store.LevelAsk) {
		lv = store.LevelAsk
	}
	reason := cleanReason(m.Reason)
	if reason == "" {
		reason = "the " + server + " integration asked for it"
	}
	return Floor{Level: lv, Reason: reason}
}

// cleanReason keeps a plugin's reason short, on one line and printable:
// it is shown to people, and it comes from outside Thicket.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '`' || r == '*' || r == '_' || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxFloorReason {
		s = string(r[:maxFloorReason]) + "…"
	}
	return s
}

type floorKey struct{ sprig, chat, scope string }

type floorEntry struct {
	Floor
	until time.Time
}

// raiseFloor records a plugin's floor for a sprig's calls to that
// integration in a chat. A stricter floor already there stays.
func (e *Engine) raiseFloor(sprig, chat, scope string, f Floor) {
	key := floorKey{sprig, chat, scope}
	for {
		now := e.now()
		old, loaded := e.floors.LoadOrStore(key, &floorEntry{Floor: f, until: now.Add(floorTTL)})
		if !loaded {
			return
		}
		cur := old.(*floorEntry)
		next := &floorEntry{Floor: f, until: now.Add(floorTTL)}
		if cur.until.After(now) && strictness(cur.Level) <= strictness(f.Level) {
			next.Floor = cur.Floor // never lowered; the stricter reason stays
			if cur.until.After(next.until) {
				next.until = cur.until // never shortened
			}
		}
		if e.floors.CompareAndSwap(key, old, next) {
			return
		}
	}
}

// floorFor returns the floor on this call, if any.
func (e *Engine) floorFor(ctx context.Context, t *turn, tool *Tool) (Floor, bool) {
	if !changes(tool) {
		return Floor{}, false
	}
	var out Floor
	found := false
	add := func(f Floor) {
		if !found || strictness(f.Level) < strictness(out.Level) {
			out = f
		}
		found = true
	}
	if strings.HasPrefix(tool.Scope, "mcp:") {
		if v, ok := e.floors.Load(floorKey{t.char.ID, t.chatRef, tool.Scope}); ok {
			if f := v.(*floorEntry); f.until.After(e.now()) {
				add(f.Floor)
			}
		}
	}
	if tool.Name != "use_skill" && tool.Name != "finish_skill" {
		run, err := e.DB.ForcedSkillRun(ctx, t.char.ID, t.chatRef, e.now().Add(-skillRunFloorAge))
		switch {
		case err == nil:
			add(Floor{Level: store.LevelAsk, Reason: fmt.Sprintf("run %d of %d of a new skill (%s)", run.Number, skillFloorRuns, run.SkillID)})
		case err != store.ErrNotFound:
			e.Log.Error("check skill runs; asking instead", "err", err)
			add(Floor{Level: store.LevelAsk, Reason: "couldn't check this sprig's skill runs"}) // fail closed
		}
	}
	return out, found
}

// noteFloor puts a floored call in the trail: the configured level, the
// floor and its reason.
func (e *Engine) noteFloor(ctx context.Context, t *turn, tool *Tool, configured string, f Floor) {
	_ = e.DB.AddTrail(ctx, store.TrailEntry{UserID: t.user.ID, Character: t.char.ID, Kind: store.TrailFloor,
		Summary: fmt.Sprintf("%s: level %s, floor %s (%s)", tool.Label(), configured, f.Level, f.Reason)})
}

// floorLine is the line under an approval prompt for a floored call.
func floorLine(f Floor) string { return "Always asks: " + f.Reason + "." }

// effectiveLevel is the level that applies to a call now: the configured
// level, made stricter by any floor on it.
func (e *Engine) effectiveLevel(ctx context.Context, t *turn, tool *Tool) string {
	lv := e.toolLevel(ctx, t.char, tool)
	if f, ok := e.floorFor(ctx, t, tool); ok {
		lv = stricter(lv, f.Level)
	}
	return lv
}
