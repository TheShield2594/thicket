package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

// The worth-saying gate. Every message a sprig starts itself (job and
// trigger output, check-ins, follow-ups) passes worthSaying once it is
// written, before it is sent or held for quiet hours (gate.go). It is held
// back when:
//
//  1. It's a rerun: it says what one of the sprig's last few proactive
//     messages to this person said (the same words, give or take a few,
//     and the same numbers). Decided in code.
//  2. Nothing is new: since the sprig last wrote to this person there is
//     nothing on the calendar for the next day and nothing newly added,
//     nothing due (follow-ups, open inbox items), no new memories, no other
//     job results, and the message itself brings none (a due follow-up, a
//     trigger's new items, outside sources a job read). Decided in code,
//     without a model.
//  3. Otherwise the cheap model reads it with what's new and the recent
//     messages and says whether it's worth saying; filler such as
//     "nothing new today" isn't. If that check can't run, it goes out.
//
// A held-back job run is recorded as empty, as nothing_to_report does; a
// held-back check-in or follow-up counts as handled. Nothing is sent
// either way, and the activity log says what was held back and why.

const (
	// repeatWithin is how many recent proactive messages a draft is
	// compared with.
	repeatWithin = 3
	// repeatAt is the word overlap (Jaccard) from which a draft counts as
	// saying the same thing.
	repeatAt = 0.7
	// newsAhead is how far ahead the calendar and due follow-ups count as
	// something to tell the person.
	newsAhead = 24 * time.Hour
)

// proactiveDraft says what a proactive message is and what it brings by
// itself.
type proactiveDraft struct {
	what  string   // for the activity log: job “Morning brief”, a check-in
	jobID int64    // the job that wrote it (its own runs aren't news), 0 for none
	news  []string // what the message itself brings, e.g. a due follow-up
}

// errNotWorth means the worth-saying gate held a proactive message back,
// so nothing was sent; the activity log says why.
var errNotWorth = errors.New("nothing new worth saying; not sent")

// worthSaying returns why c shouldn't send u text, or "" when it should.
// ctx carries the model-call meta of the work that wrote it.
func (e *Engine) worthSaying(ctx context.Context, u *store.User, c *character.Character, text string, d proactiveDraft) string {
	recent, err := e.DB.RecentSaid(ctx, u.ID, c.ID, repeatWithin)
	if err != nil {
		e.Log.Warn("read recent proactive messages", "err", err) // compared with nothing: it goes out
	}
	for _, r := range recent {
		if repeats(text, r.Text) {
			return "it repeats what " + c.DisplayName() + " said " + r.SentAt.In(u.Location()).Format("Mon 2 Jan 15:04")
		}
	}
	found, since := e.newsFor(ctx, u, c, d.jobID)
	news := append(append([]string(nil), d.news...), found...)
	if len(news) == 0 {
		return "nothing new since " + c.DisplayName() + " last wrote (" + since.In(u.Location()).Format("Mon 2 Jan 15:04") +
			"): nothing on the calendar, nothing due, no new memories or job results"
	}
	return e.judgeWorth(ctx, u, c, text, news, recent)
}

// newsFor lists what changed for u that c could be telling them about
// since c last wrote to them, which it returns too. Anything it can't
// check counts as news, so a database or calendar hiccup never silences a
// sprig.
func (e *Engine) newsFor(ctx context.Context, u *store.User, c *character.Character, jobID int64) (news []string, since time.Time) {
	since, err := e.DB.LastProactiveAt(ctx, u.ID, c.ID)
	if err != nil {
		return []string{"couldn't tell when " + c.DisplayName() + " last wrote"}, since
	}
	if since.IsZero() {
		return []string{c.DisplayName() + " hasn't written to them before"}, since
	}
	now := e.now()
	note := func(n int, what string) {
		if n > 0 {
			news = append(news, fmt.Sprintf("%d %s", n, what))
		}
	}

	// The calendar: what's coming up, and what sprigs newly put on it.
	if !u.IsKid() {
		n, errs, err := e.upcomingItems(ctx, u, now, now.Add(newsAhead))
		switch {
		case err != nil:
			news = append(news, "couldn't read the calendar")
		case len(errs) > 0:
			news = append(news, "couldn't read "+strings.Join(errs, "; "))
		}
		note(n, "calendar items in the next day")
	}
	if evs, err := e.DB.SprigEventsFor(ctx, u.ID, now, now.AddDate(0, 0, 60)); err != nil {
		news = append(news, "couldn't read sprig calendar entries")
	} else {
		added := 0
		for _, ev := range evs {
			if ev.CreatedAt.After(since) {
				added++
			}
		}
		note(added, "new calendar entries")
	}

	// Due things and new memories.
	if mems, err := e.DB.VisibleMemories(ctx, u.ID, c.ID); err != nil {
		news = append(news, "couldn't read memories")
	} else {
		due, fresh := 0, 0
		for _, m := range mems {
			if m.Kind == "followup" && !m.DueAt.IsZero() && m.DueAt.Before(now.Add(newsAhead)) {
				due++
			}
			if m.CreatedAt.After(since) || m.UpdatedAt.After(since) {
				fresh++
			}
		}
		note(due, "follow-ups due")
		note(fresh, "new or changed memories")
	}
	if items, err := e.DB.OpenInbox(ctx, u.ID); err != nil {
		news = append(news, "couldn't read the inbox")
	} else {
		n := 0
		for _, it := range items {
			if it.UpdatedAt.After(since) {
				n++
			}
		}
		note(n, "new things waiting for them")
	}

	// Results of other jobs.
	if runs, err := e.DB.JobRunsBetween(ctx, u.ID, since, now.Add(time.Second)); err != nil {
		news = append(news, "couldn't read job results")
	} else {
		n := 0
		for _, r := range runs {
			if r.Status == "ok" && r.JobID != jobID {
				n++
			}
		}
		note(n, "job results")
	}
	return news, since
}

// upcomingItems counts what's on u's calendar between from and to: their
// connected calendars and what sprigs put on it. errs names calendars
// that couldn't be read.
func (e *Engine) upcomingItems(ctx context.Context, u *store.User, from, to time.Time) (int, []string, error) {
	if e.Calendar == nil {
		evs, err := e.DB.SprigEventsFor(ctx, u.ID, from, to)
		return len(evs), nil, err
	}
	items, errs, err := e.Calendar.View(ctx, u, from, to, calendar.Filter{Layers: map[string]bool{calendar.LayerEvents: true, calendar.LayerOutputs: true}})
	return len(items), errs, err
}

// jobDraft is what a job run's output brings by itself: the new items a
// trigger or webhook fired on, and outside sources the run read (which
// only a model can judge).
func jobDraft(j *store.Job, t *turn) proactiveDraft {
	d := proactiveDraft{what: fmt.Sprintf("job “%s”", j.Name), jobID: j.ID}
	if j.Payload != "" || j.Kind == store.JobWebhook {
		from := j.Event
		if from == "" {
			from = "a webhook call"
		}
		d.news = append(d.news, "it fired on new items from "+from)
	}
	var read []string
	for _, src := range t.taintedBy {
		// The calendar is checked by the gate itself; the event a job fired
		// on is above.
		if src == "calendar" || strings.HasPrefix(src, "webhook or event: ") {
			continue
		}
		read = append(read, src)
	}
	if len(read) > 0 {
		d.news = append(d.news, "it read "+strings.Join(read, ", "))
	}
	return d
}

// words is the set of lowercase words and numbers in s.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out[w] = true
	}
	return out
}

// similarity is the word overlap of a and b: shared words over all words
// (Jaccard), 0 to 1.
func similarity(a, b string) float64 {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	both := 0
	for w := range wa {
		if wb[w] {
			both++
		}
	}
	return float64(both) / float64(len(wa)+len(wb)-both)
}

// repeats reports whether a says what b said: nearly the same words, and
// exactly the same numbers (times, dates and amounts are what change from
// one day's message to the next).
func repeats(a, b string) bool {
	if similarity(a, b) < repeatAt {
		return false
	}
	na, nb := numbers(a), numbers(b)
	if len(na) != len(nb) {
		return false
	}
	for n := range na {
		if !nb[n] {
			return false
		}
	}
	return true
}

func numbers(s string) map[string]bool {
	out := map[string]bool{}
	for w := range words(s) {
		if strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			out[w] = true
		}
	}
	return out
}

const worthRubric = `A character in a personal assistant app is about to message someone without being asked. Decide whether the message is worth sending.
Worth it: it tells them something new or useful now (what's coming up, something due, a result, a follow-up on something they mentioned), or it's a warm check-in that doesn't rerun a recent message.
Not worth it: filler; a message whose point is that there's nothing new, nothing planned or nothing to report; or one that says again what a recent message already said.
Reply with JSON only: {"worth": true|false, "why": "one short sentence"}`

// judgeWorth asks the cheap model whether text is worth sending, given
// what's new and the recent messages. It returns why not, or "" to send;
// a check that fails to run sends.
func (e *Engine) judgeWorth(ctx context.Context, u *store.User, c *character.Character, text string, news []string, recent []store.Said) string {
	var b strings.Builder
	b.WriteString("What changed since the character last wrote to them:\n- " + strings.Join(news, "\n- "))
	if len(recent) > 0 {
		b.WriteString("\n\nThe character's recent messages to them, newest first:")
		for _, r := range recent {
			fmt.Fprintf(&b, "\n- (%s) %s", r.SentAt.In(u.Location()).Format("Mon 2 Jan 15:04"), truncate(r.Text, 600))
		}
	}
	b.WriteString("\n\nThe message it's about to send:\n" + truncate(text, 2000))
	temp := 0.0
	resp, err := e.LLM.Chat(llm.WithPurpose(ctx, "presence"), llm.Request{
		Model:       e.Settings.String(ctx, "cheap_model"),
		Messages:    []llm.Message{{Role: "system", Content: worthRubric}, {Role: "user", Content: b.String()}},
		Temperature: &temp, MaxTokens: 150, JSON: true, NoFallback: true,
	})
	if err != nil {
		e.Log.Warn("worth-saying check failed to run; sending", "user", u.ID, "character", c.ID, "err", err)
		return ""
	}
	var v struct {
		Worth *bool  `json:"worth"`
		Why   string `json:"why"`
	}
	s := strings.TrimSpace(resp.Message.Content)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```"))
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Worth == nil {
		e.Log.Warn("unreadable worth-saying verdict; sending", "verdict", truncate(s, 200))
		return ""
	}
	if *v.Worth {
		return ""
	}
	why := strings.TrimSpace(v.Why)
	if why == "" {
		why = "nothing in it is new or useful"
	}
	return "not worth saying: " + why
}
