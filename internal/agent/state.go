package agent

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/character"
)

// What each sprig is doing right now, for its avatar: working (running a
// tool), thinking (waiting on the model), pleased or unsure (how its last
// reply went, for a few minutes: see mood.go), stuck (failed or
// interrupted work in the inbox from the last day), sleeping (its quiet
// hours, in the owner's timezone), else idle.

type busy struct {
	thinking, working atomic.Int32

	mu       sync.Mutex
	verdicts map[int64]Verdict // conversation id -> last reply's verdict
	waiting  time.Time         // when it last asked for an approval still unanswered
}

func (e *Engine) busyFor(id string) *busy {
	v, _ := e.activity.LoadOrStore(id, &busy{})
	return v.(*busy)
}

// mark counts the sprig as busy until the returned func is called.
func (e *Engine) mark(id string, working bool) func() {
	b := e.busyFor(id)
	n := &b.thinking
	if working {
		n = &b.working
	}
	n.Add(1)
	var once sync.Once
	return func() { once.Do(func() { n.Add(-1) }) }
}

// SprigStates returns each sprig's state by id.
func (e *Engine) SprigStates(ctx context.Context) map[string]string {
	out := map[string]string{}
	var owner *time.Location
	if admins, err := e.DB.Admins(ctx); err == nil && len(admins) > 0 {
		owner = admins[0].Location()
	}
	for _, c := range e.Chars.All() {
		out[c.ID] = e.sprigState(ctx, c, owner)
	}
	return out
}

func (e *Engine) sprigState(ctx context.Context, c *character.Character, loc *time.Location) string {
	b := e.busyFor(c.ID)
	switch {
	case b.working.Load() > 0:
		return "working"
	case b.thinking.Load() > 0:
		return "thinking"
	}
	if m := e.sprigMood(c.ID); m != "" {
		return m
	}
	if n, err := e.DB.OpenInboxFor(ctx, c.ID, e.now().Add(-24*time.Hour)); err == nil && n > 0 {
		return "stuck"
	}
	if loc != nil {
		n := e.now().In(loc)
		if c.InQuietHours(n.Hour()*60 + n.Minute()) {
			return "sleeping"
		}
	}
	return "idle"
}

// --- the commons ---

// The commons is a shared channel (THICKET_COMMONS_CHANNEL) where sprigs
// hand work to each other in the open: a sprig that @mentions another in
// its reply hands the conversation to it, and every handoff is logged to
// the trail.

func (e *Engine) isCommons(chatRef string) bool {
	return e.Cfg != nil && e.Cfg.CommonsRef != "" && chatRef == e.Cfg.CommonsRef
}

var mentionRe = regexp.MustCompile(`(?:^|[^\w@])@([A-Za-z][A-Za-z0-9-]*)`)

// mentionedSprig is the first other sprig @mentioned in text.
func (e *Engine) mentionedSprig(text string, self *character.Character) *character.Character {
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		if c, ok := e.Chars.FindByName(m[1]); ok && c.ID != self.ID {
			return c
		}
	}
	return nil
}

const commonsGuide = `
## The commons
You're in the commons, the channel where sprigs work together in the open. To hand something to another sprig,
@mention them by name in your reply (e.g. "@Pip, can you check the calendar for Friday?") and say exactly what you need:
they pick it up right after your message. Only mention a sprig when you're handing them something.
`

func commonsSection(on bool) string {
	if !on {
		return ""
	}
	return strings.TrimSpace(commonsGuide) + "\n"
}
