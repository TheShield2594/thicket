package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Check-in presets: a sprig speaks first. Switched on per sprig in its
// character file (initiative.*) or in chat (set_initiative):
//
//   - morning_brief: at a set time in each person's timezone, a short
//     brief of today's calendar and anything due;
//   - event_reminders: a heads-up some minutes before each calendar event
//     ("you have a thing at 3"), once per event and person, whichever
//     sprig gets there first;
//   - due_reminders: a reminder when something noted as due (a follow-up
//     memory with a due time) is coming up, once per item.
//
// These are reaching out, not running a task: what to say is gathered in
// code (the merged calendar, follow-up memories), the sprig writes it in
// its own voice without tools, and it goes the way every proactive message
// goes (proactive): quiet hours and the daily budget, the worth-saying
// gate, the person's chosen app. Each is claimed before it's written, so
// it's sent once; one held back by quiet hours or the budget is released
// and tried on a later round. Kid sprigs never do any of this.
//
// Who gets them: the adults a sprig is for (its person, for a solo sprig;
// everyone who has used Thicket, for a family sprig).

const (
	// briefWindow is how long after its time a morning brief still goes
	// out (the initiative loop runs every few minutes; a restart or quiet
	// hours can make it late).
	briefWindow = 2 * time.Hour
	// dueLead is how far ahead a due reminder comes; dueGrace how late.
	dueLead  = 3 * time.Hour
	dueGrace = time.Hour
)

// hasPresets reports whether a sprig has any check-in preset on.
func hasPresets(c *character.Character) bool {
	in := c.Initiative
	return in.MorningBrief != "" || in.EventReminders > 0 || in.DueReminders
}

// Presets runs the check-in presets that are due. The initiative loop
// calls it every few minutes; tests call it with a fake clock.
func (e *Engine) Presets(ctx context.Context) {
	var sprigs []*character.Character
	for _, c := range e.Chars.All() {
		if hasPresets(c) && !c.IsKid() {
			sprigs = append(sprigs, c)
		}
	}
	if len(sprigs) == 0 || e.paused(ctx, "") != nil {
		return
	}
	users, err := e.DB.ListUsers(ctx)
	if err != nil {
		e.Log.Error("list users for check-ins", "err", err)
		return
	}
	for _, c := range sprigs {
		if e.paused(ctx, c.ID) != nil || e.Budget.HeartbeatPause(ctx, c.ID) != "" {
			continue
		}
		for i := range users {
			u := &users[i]
			if u.IsKid() || !c.UsableBy(u) || u.LastSeenAt.IsZero() && c.BelongsTo != u.ID {
				continue // never used Thicket: nobody to brief yet
			}
			if c.Initiative.MorningBrief != "" {
				e.morningBrief(ctx, c, u)
			}
			if c.Initiative.EventReminders > 0 {
				e.eventReminders(ctx, c, u)
			}
			if c.Initiative.DueReminders {
				e.dueReminders(ctx, c, u)
			}
		}
	}
}

// presetSent handles what came of sending a preset message. Sent, or
// held back as not worth saying: done. Anything else (held for quiet
// hours, paused, a model or delivery failure) frees its claim, so a later
// round tries again. A failed delivery may rarely have arrived after all;
// a second copy is better than a lost reminder.
func (e *Engine) presetSent(ctx context.Context, key string, err error) {
	if err != nil && !errors.Is(err, errNotWorth) {
		_ = e.DB.ReleaseNotice(context.WithoutCancel(ctx), key)
	}
}

// calendarFor is a person's merged calendar (their own and shared
// calendars, and what sprigs put on it) between from and to. Jobs aren't
// news.
func (e *Engine) calendarFor(ctx context.Context, u *store.User, from, to time.Time) ([]calendar.Item, []string) {
	if e.Calendar == nil {
		return nil, nil
	}
	items, errs, err := e.Calendar.View(ctx, u, from, to, calendar.Filter{Layers: map[string]bool{calendar.LayerEvents: true, calendar.LayerOutputs: true}})
	if err != nil {
		return nil, []string{err.Error()}
	}
	return items, errs
}

func (e *Engine) morningBrief(ctx context.Context, c *character.Character, u *store.User) {
	at, err := character.ParseClock(c.Initiative.MorningBrief)
	if err != nil {
		return
	}
	loc := u.Location()
	now := e.now().In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), at/60, at%60, 0, 0, loc) // the clock time, even on a DST day
	if now.Before(start) || !now.Before(start.Add(briefWindow)) {
		return
	}
	if e.holdReason(ctx, c, u) != "" {
		return // quiet hours or the budget: tried again on the next round, while the window lasts
	}
	key := fmt.Sprintf("brief:%s:%d:%s", c.ID, u.ID, day.Format("2006-01-02"))
	if ok, _ := e.DB.ClaimOnce(ctx, key); !ok {
		return
	}
	items, errs := e.calendarFor(ctx, u, day, day.AddDate(0, 0, 1))
	due, err := e.DB.FollowupsDueBetween(ctx, u.ID, c.ID, day, day.AddDate(0, 0, 1), e.now(), "")
	if err != nil {
		errs = append(errs, "follow-ups: "+err.Error())
	}
	var b strings.Builder
	var news []string
	fmt.Fprintf(&b, "[Morning brief] It's %s morning for them (%s). ", now.Format("Monday 2 January"), now.Format("15:04"))
	if len(items) > 0 {
		b.WriteString("Today on their calendar:\n")
		for _, it := range items {
			line := eventLine(it, loc)
			fmt.Fprintf(&b, "- %s\n", line)
			news = append(news, "calendar today: "+line)
		}
	} else {
		b.WriteString("Nothing is on their calendar today.\n")
	}
	if len(due) > 0 {
		b.WriteString("Due today:\n")
		for _, m := range due {
			line := fmt.Sprintf("%s (due %s)", m.Content, m.DueAt.In(loc).Format("15:04"))
			fmt.Fprintf(&b, "- %s\n", line)
			news = append(news, "due today: "+line)
		}
	}
	if len(errs) > 0 {
		fmt.Fprintf(&b, "Some of it couldn't be read: %s. Say so in a few words.\n", strings.Join(errs, "; "))
		news = append(news, "a calendar couldn't be read")
	}
	b.WriteString("Write them a short morning brief in your own voice: today's calendar and anything due, in a few lines, then one friendly line. " +
		"Only what's listed here: make nothing up. If the day is clear and nothing is due, say good morning and that the day's clear, in one line.")
	err = e.proactive(ctx, u, c, b.String(), proactiveDraft{what: "the morning brief", news: news})
	e.presetSent(ctx, key, err)
}

// eventLine is one calendar item for a brief or a heads-up.
func eventLine(it calendar.Item, loc *time.Location) string {
	when := "all day"
	if !it.AllDay {
		when = it.Start.In(loc).Format("15:04")
	}
	line := fmt.Sprintf("%s %s", when, it.Title)
	if it.Location != "" {
		line += " (at " + it.Location + ")"
	}
	return line
}

func (e *Engine) eventReminders(ctx context.Context, c *character.Character, u *store.User) {
	lead := time.Duration(c.Initiative.EventReminders) * time.Minute
	now := e.now()
	items, _ := e.calendarFor(ctx, u, now, now.Add(lead))
	loc := u.Location()
	for _, it := range items {
		if it.AllDay || !it.Start.After(now) || it.Start.After(now.Add(lead)) {
			continue
		}
		if e.holdReason(ctx, c, u) != "" {
			return
		}
		// Once per event and person, whichever sprig gets there first.
		key := fmt.Sprintf("event:%d:%s", u.ID, it.ID)
		if ok, _ := e.DB.ClaimOnce(ctx, key); !ok {
			continue
		}
		mins := int(it.Start.Sub(now).Round(time.Minute) / time.Minute)
		where := ""
		if it.Location != "" {
			where = ", at " + it.Location
		}
		event := fmt.Sprintf("[Event heads-up] They have %q at %s today (in %d minutes%s). Give them a one-line heads-up in your own voice. Nothing else.",
			it.Title, it.Start.In(loc).Format("15:04"), mins, where)
		err := e.proactive(ctx, u, c, event, proactiveDraft{what: "an event heads-up", news: []string{"calendar soon: " + eventLine(it, loc)}})
		e.presetSent(ctx, key, err)
	}
}

func (e *Engine) dueReminders(ctx context.Context, c *character.Character, u *store.User) {
	now := e.now()
	// Ones already reminded about are left out before the query's limit,
	// so a long list is worked through.
	due, err := e.DB.FollowupsDueBetween(ctx, u.ID, c.ID, now.Add(-dueGrace), now.Add(dueLead), now, "due:")
	if err != nil {
		e.Log.Warn("due reminders", "err", err)
		return
	}
	loc := u.Location()
	for _, m := range due {
		if e.holdReason(ctx, c, u) != "" {
			return
		}
		key := fmt.Sprintf("due:%d", m.ID)
		if ok, _ := e.DB.ClaimOnce(ctx, key); !ok {
			continue
		}
		when := m.DueAt.In(loc).Format("15:04")
		if m.DueAt.In(loc).Format("2006-01-02") != now.In(loc).Format("2006-01-02") {
			when = m.DueAt.In(loc).Format("Monday 15:04")
		}
		event := fmt.Sprintf("[Due reminder] They noted this is due at %s: %q. Remind them in one short line, in your own voice.", when, m.Content)
		err := e.proactive(ctx, u, c, event, proactiveDraft{what: "a due reminder", news: []string{"due soon: " + truncate(m.Content, 200)}})
		e.presetSent(ctx, key, err)
	}
}
