package agent

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// How a sprig's last reply went, for its avatar: pleased after a reply
// that landed in character, unsure after one rewritten for drift, one the
// personality check read as unsure (a refusal, a hedge, an apology), or
// while an approval it asked for is waiting. Moods are kept in memory per
// sprig per conversation and fade back to idle after moodDecay.

const moodDecay = 5 * time.Minute

// Verdict is the personality check's view of a sprig's last reply in one
// conversation.
type Verdict struct {
	// InCharacter is false when the reply drifted and was rewritten.
	InCharacter bool
	// Mood is "pleased" or "unsure".
	Mood string
	At   time.Time
}

// readMood turns the check's mood read into a state: anything but an
// explicit "unsure" from a reply that stayed in character counts as
// landed.
func readMood(inCharacter bool, mood string) string {
	if !inCharacter || strings.EqualFold(strings.TrimSpace(mood), "unsure") {
		return "unsure"
	}
	return "pleased"
}

// noteReply keeps the verdict on t's reply. mood is the check's read ("" if
// it didn't run).
func (e *Engine) noteReply(t *turn, inCharacter bool, mood string) {
	var conv int64
	if t.conv != nil {
		conv = t.conv.ID
	}
	now := e.now()
	b := e.busyFor(t.char.ID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.verdicts == nil {
		b.verdicts = map[int64]Verdict{}
	}
	for id, v := range b.verdicts {
		if !fresh(now, v.At) {
			delete(b.verdicts, id)
		}
	}
	b.verdicts[conv] = Verdict{InCharacter: inCharacter, Mood: readMood(inCharacter, mood), At: now}
}

// LastVerdict is the verdict on a sprig's last reply in a conversation (0
// for replies outside one, like scheduled jobs), if there was one.
func (e *Engine) LastVerdict(charID string, convID int64) (Verdict, bool) {
	b := e.busyFor(charID)
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.verdicts[convID]
	return v, ok
}

// noteWaiting marks the sprig as waiting on an approval it asked for.
func (e *Engine) noteWaiting(charID string) {
	b := e.busyFor(charID)
	b.mu.Lock()
	b.waiting = e.now()
	b.mu.Unlock()
}

// doneWaiting clears the mark once the approval is answered.
func (e *Engine) doneWaiting(charID string) {
	b := e.busyFor(charID)
	b.mu.Lock()
	b.waiting = time.Time{}
	b.mu.Unlock()
}

// sprigMood is the sprig's mood now: unsure while a recent approval waits,
// else the mood of its newest reply in any conversation, "" once faded.
func (e *Engine) sprigMood(charID string) string {
	now := e.now()
	b := e.busyFor(charID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if fresh(now, b.waiting) {
		return "unsure"
	}
	var last Verdict
	for _, v := range b.verdicts {
		if v.At.After(last.At) {
			last = v
		}
	}
	if !fresh(now, last.At) {
		return ""
	}
	return last.Mood
}

// fresh reports whether a mood set at t still shows. One from after now
// (the clock was set back) has faded too.
func fresh(now, t time.Time) bool {
	age := now.Sub(t)
	return !t.IsZero() && age >= 0 && age < moodDecay
}

// moodAvatar is the picture for the sprig's mood, when one sits next to
// its avatar file: pip.png → pip.pleased.png, pip.unsure.png. Discord
// shows it per message; Telegram has no per-message pictures.
func (e *Engine) moodAvatar(charID, file string) string {
	mood := e.sprigMood(charID)
	if mood == "" {
		return ""
	}
	ext := filepath.Ext(file)
	name := strings.TrimSuffix(file, ext) + "." + mood + ext
	if _, err := os.Stat(filepath.Join(e.Chars.Dir(), name)); err != nil {
		return ""
	}
	return name
}
