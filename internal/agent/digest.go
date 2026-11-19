package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// The weekly digest. Once a week (digest.day at digest.time, Sunday 18:00
// by default, in each adult's own timezone) the host sends every adult one
// message: per sprig they can see, what it did (job runs, replies, what it
// added or changed, from the trail), what's waiting on them (open inbox
// items and approvals), and what it cost that week against its cap.
//
// It is built from the database in code. No model writes any of it, so
// the numbers and the needs-you list are exactly what the database says.
//
// Who sees what follows the household rules, using the same checks as
// everywhere else (character.UsableBy, IsParent, PrivateFrom):
//
//   - each adult: their own solo sprigs and the family sprigs;
//   - parents: also the kid sprigs they set up;
//   - nobody: another adult's solo sprig, not even its name or cost.
//
// Job runs and inbox items are the person's own. Trail entries of a
// family sprig are the person's own unless they are an admin, as on the
// dashboard. Kids get no digest.
//
// Costs follow the dashboard, where only admins see what a sprig costs
// in all: an admin sees a family sprig's whole week against its cap;
// anyone else sees only their own share of it (the spend recorded for
// them), never the family total or the cap it is measured against. The
// owner of a solo sprig and the parents of a kid sprig see its whole
// cost. The total is the sum of what the person was shown.
//
// It is proactive (it reaches people where set_reach says, waits out the
// host's quiet hours, and nothing goes out while Thicket or the host is
// paused) but exempt from the daily budget: it never waits for it and
// doesn't count against it.

// digestWindow is how long after its time a digest may still go out
// (quiet hours, a pause or a restart can push it back). Past that, that
// week's is skipped.
const digestWindow = 24 * time.Hour

// digestListMax is how many items of one kind a sprig's section lists.
const digestListMax = 3

// Digest is one person's week.
type Digest struct {
	From, To time.Time // [From, To), in the person's timezone
	Sprigs   []DigestSprig
	// Other is open inbox items and approvals of the person's that belong
	// to no sprig in the digest.
	Other []string
	// TotalUSD is the sum of the sprigs' CostUSD: what the person is shown.
	TotalUSD float64
}

// Shares reports whether any sprig's cost is only the person's share.
func (d *Digest) Shares() bool {
	for _, s := range d.Sprigs {
		if s.Share {
			return true
		}
	}
	return false
}

// DigestSprig is one sprig's week, as one person may see it.
type DigestSprig struct {
	ID, Name string
	Tier     string
	// For names whose sprig it is ("yours", or the kids it talks to).
	For     string
	Runs    int // job runs that started this week (the person's jobs)
	Failed  int // of them, failed
	Replies int // replies it wrote to the person (to the kids, for a kid sprig)
	Changes []string
	Safety  int      // safety alerts (kid sprigs)
	Waiting []string // open inbox items and approvals
	// CostUSD is its whole week's spend, or, when Share is set (a family
	// sprig, for someone who isn't an admin), only what was spent for
	// the person. A share comes with no cap.
	CostUSD  float64
	Share    bool
	WeekCap  float64 // its daily cap × 7 (there is no weekly cap setting); 0 = no cap of its own
	DailyCap float64
}

// Empty reports whether the sprig has nothing to report this week.
func (d DigestSprig) Empty() bool {
	return d.Runs == 0 && d.Replies == 0 && len(d.Changes) == 0 && d.Safety == 0 && len(d.Waiting) == 0 && d.CostUSD < 0.005
}

// Empty reports whether the week has nothing to say.
func (d *Digest) Empty() bool {
	for _, s := range d.Sprigs {
		if !s.Empty() {
			return false
		}
	}
	return len(d.Other) == 0
}

// digestSprigs are the sprigs u's digest covers: their own solo sprigs,
// the family sprigs, and the kid sprigs they set up. Never someone else's
// solo sprig.
func (e *Engine) digestSprigs(u *store.User) []*character.Character {
	var out []*character.Character
	if u.IsKid() {
		return nil
	}
	for _, c := range e.Chars.All() {
		if c.PrivateFrom(u.ID) {
			continue
		}
		if c.UsableBy(u) || c.IsParent(u.ID) {
			out = append(out, c)
		}
	}
	return out
}

// BuildDigest gathers u's week [from, to) from the database.
func (e *Engine) BuildDigest(ctx context.Context, u *store.User, from, to time.Time) (*Digest, error) {
	d := &Digest{From: from.In(u.Location()), To: to.In(u.Location())}
	chars := e.digestSprigs(u)
	if len(chars) == 0 {
		return d, nil
	}
	host, _ := e.Chars.Host()
	idx := map[string]int{}
	for i, c := range chars {
		idx[c.ID] = i
		s := DigestSprig{ID: c.ID, Name: c.DisplayName(), Tier: c.Tier()}
		switch c.Tier() {
		case character.TierSolo:
			s.For = "yours"
		case character.TierKid:
			s.For = "for " + e.kidNames(ctx, c.Kids)
		}
		var cost float64
		var err error
		if c.Tier() == character.TierFamily && !u.IsAdmin() {
			// Only their share, as the dashboard shows costs to admins only.
			s.Share = true
			cost, err = e.DB.UserSpendBetween(ctx, c.ID, u.ID, from, to)
		} else {
			lim := e.Settings.Float(ctx, "budget.character."+c.ID+".daily_usd")
			s.DailyCap, s.WeekCap = lim, 7*lim
			cost, err = e.DB.SpendBetween(ctx, c.ID, from, to)
		}
		if err != nil {
			return nil, fmt.Errorf("spend for %s: %w", c.ID, err)
		}
		s.CostUSD = cost
		d.TotalUSD += cost
		people := []int64{u.ID}
		if c.IsKid() {
			people = c.Kids
		}
		if s.Replies, err = e.DB.RepliesBetween(ctx, c.ID, people, from, to); err != nil {
			return nil, fmt.Errorf("replies for %s: %w", c.ID, err)
		}
		d.Sprigs = append(d.Sprigs, s)
	}
	// sprigOf maps an item's sprig to its section: no sprig means the host.
	sprigOf := func(id string) (int, bool) {
		if id == "" && host != nil {
			id = host.ID
		}
		i, ok := idx[id]
		return i, ok
	}

	// Job runs: the person's own jobs.
	jobs, err := e.DB.UserJobs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	jobChar := map[int64]string{}
	for _, j := range jobs {
		jobChar[j.ID] = j.Character
	}
	runs, err := e.DB.JobRunsBetween(ctx, u.ID, from, to)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		i, ok := sprigOf(jobChar[r.JobID])
		if !ok || r.Status == "missed" || r.Status == "running" {
			continue
		}
		d.Sprigs[i].Runs++
		if r.Status == "error" {
			d.Sprigs[i].Failed++
		}
	}

	// What was added or changed, from the trail.
	trail, err := e.DB.TrailBetween(ctx, from, to)
	if err != nil {
		return nil, err
	}
	for _, t := range trail {
		i, ok := idx[t.Character]
		if !ok {
			continue
		}
		c := chars[i]
		// A family sprig's entries: the person's own, or every one for an
		// admin (as on the dashboard). Their own solo sprig's and the kid
		// sprigs they parent: all of them.
		if c.Tier() == character.TierFamily && t.UserID != u.ID && !u.IsAdmin() {
			continue
		}
		switch t.Kind {
		case store.TrailSafety:
			d.Sprigs[i].Safety++
		case store.TrailUndo, store.TrailUpdate:
		default:
			d.Sprigs[i].Changes = append(d.Sprigs[i].Changes, shorten(t.Summary, 90))
		}
	}

	// What's waiting on the person: open inbox items and approvals.
	items, err := e.DB.OpenInbox(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		what := shorten(it.Title, 90)
		if it.Count > 1 {
			what += fmt.Sprintf(" (×%d)", it.Count)
		}
		e.digestWaiting(d, sprigOf, it.Character, u.ID, what)
	}
	apps, err := e.DB.PendingApprovals(ctx, u.ID, approvalMaxAge)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		tool := a.Tool
		if strings.HasPrefix(tool, "thicket:") {
			tool = strings.ReplaceAll(strings.TrimPrefix(tool, "thicket:"), "_", " ")
		}
		e.digestWaiting(d, sprigOf, a.Character, u.ID, "approve "+tool)
	}
	return d, nil
}

// digestWaiting files something waiting on the person under its sprig, or
// under Other. Something about another adult's solo sprig is left out.
func (e *Engine) digestWaiting(d *Digest, sprigOf func(string) (int, bool), char string, userID int64, what string) {
	if i, ok := sprigOf(char); ok {
		d.Sprigs[i].Waiting = append(d.Sprigs[i].Waiting, what)
		return
	}
	if e.Chars.PrivateFrom(char, userID) {
		return
	}
	d.Other = append(d.Other, what)
}

// kidNames names a kid sprig's kids: "Max", "Max and Ada".
func (e *Engine) kidNames(ctx context.Context, ids []int64) string {
	var names []string
	for _, id := range ids {
		if k, err := e.DB.UserByID(ctx, id); err == nil && k.Name != "" {
			names = append(names, k.Name)
		}
	}
	switch len(names) {
	case 0:
		return "the kids"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Text writes the digest for a phone: a few lines per sprig with
// something to say, the total, and the dashboard link. link may be "".
func (d *Digest) Text(link string) string {
	span := d.From.Format("Mon 2 Jan") + " – " + d.To.Add(-time.Minute).Format("Mon 2 Jan")
	more := ""
	if link != "" {
		more = "\nMore on the dashboard: " + link
	}
	if d.Empty() {
		return "**Your week** (" + span + "): a quiet one. Nothing ran, nothing changed and nothing is waiting on you." + more
	}
	var b strings.Builder
	b.WriteString("**Your week** (" + span + ")\n")
	for _, s := range d.Sprigs {
		if s.Empty() {
			continue
		}
		b.WriteString("\n**" + s.Name + "**")
		if s.For != "" {
			b.WriteString(" (" + s.For + ")")
		}
		switch {
		case s.Share:
			b.WriteString(" · your share " + digestMoney(s.CostUSD))
		case s.WeekCap > 0:
			b.WriteString(fmt.Sprintf(" · %s of a %s week (7 × %s daily cap)", digestMoney(s.CostUSD), digestMoney(s.WeekCap), digestMoney(s.DailyCap)))
		default:
			b.WriteString(" · " + digestMoney(s.CostUSD) + ", no cap of its own")
		}
		b.WriteString("\n")
		var did []string
		if s.Runs > 0 {
			r := countOf(s.Runs, "job run", "job runs")
			if s.Failed > 0 {
				r += fmt.Sprintf(" (%d failed)", s.Failed)
			}
			did = append(did, r)
		}
		if s.Replies > 0 {
			to := "you"
			if s.Tier == character.TierKid {
				to = strings.TrimPrefix(s.For, "for ")
			}
			did = append(did, countOf(s.Replies, "reply", "replies")+" to "+to)
		}
		if s.Safety > 0 {
			did = append(did, countOf(s.Safety, "safety alert", "safety alerts")+" (sent to you at the time)")
		}
		if len(did) > 0 {
			b.WriteString("Did: " + strings.Join(did, "; ") + "\n")
		}
		if len(s.Changes) > 0 {
			b.WriteString("Changed: " + digestList(s.Changes) + "\n")
		}
		if len(s.Waiting) > 0 {
			b.WriteString("Needs you: " + digestList(s.Waiting) + "\n")
		}
	}
	if len(d.Other) > 0 {
		b.WriteString("\nAlso waiting on you: " + digestList(d.Other) + "\n")
	}
	if d.Shares() {
		b.WriteString("\nTotal: " + digestMoney(d.TotalUSD) + " this week, with your share of the family sprigs.")
	} else {
		b.WriteString("\nTotal: " + digestMoney(d.TotalUSD) + " for these sprigs this week.")
	}
	b.WriteString(more)
	return b.String()
}

// digestList joins up to digestListMax items, saying how many more there are.
func digestList(items []string) string {
	if len(items) <= digestListMax {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:digestListMax], "; ") + fmt.Sprintf("; and %d more", len(items)-digestListMax)
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// digestMoney is money for the digest: tiny amounts read "<$0.01", not "$0.00".
func digestMoney(v float64) string {
	if v > 0 && v < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// shorten cuts s to n runes on one line.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// digestSchedule is when the digest goes out: the weekday and minute of
// the day, or ok false when it's off.
func (e *Engine) digestSchedule(ctx context.Context) (day time.Weekday, minute int, ok bool) {
	name := strings.ToLower(e.Settings.String(ctx, "digest.day"))
	found := false
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.ToLower(d.String()) == name {
			day, found = d, true
		}
	}
	if !found {
		return 0, 0, false
	}
	h, m, err := settings.Clock(e.Settings.String(ctx, "digest.time"))
	if err != nil {
		h, m, _ = settings.Clock(e.Settings.Default("digest.time"))
	}
	return day, h*60 + m, true
}

// DigestSchedule is the household's digest day ("sunday") and time
// ("18:00"), or "off" and "" when it's off. For the API and the web app.
func (e *Engine) DigestSchedule(ctx context.Context) (day, at string) {
	d, minute, on := e.digestSchedule(ctx)
	if !on {
		return "off", ""
	}
	return strings.ToLower(d.String()), fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// SetDigestModeFor stores how u wants the weekly digest, as the
// weekly_digest tool does, outside chat (the API and the web app).
func (e *Engine) SetDigestModeFor(ctx context.Context, u *store.User, mode string) error {
	if u.IsKid() {
		return fmt.Errorf("the weekly digest is for adults")
	}
	if !store.ValidDigestMode(mode) {
		return fmt.Errorf("mode is always, skip_empty or off, not %q", mode)
	}
	if err := e.DB.SetDigestMode(ctx, u.ID, mode); err != nil {
		return err
	}
	e.logActivity(ctx, "info", "system", "", u.ID, "weekly digest: "+mode, "")
	return nil
}

// lastDigestTime is the latest digest time at or before now, in loc.
func lastDigestTime(now time.Time, loc *time.Location, day time.Weekday, minute int) time.Time {
	l := now.In(loc)
	back := (int(l.Weekday()) - int(day) + 7) % 7
	t := time.Date(l.Year(), l.Month(), l.Day()-back, minute/60, minute%60, 0, 0, loc)
	if t.After(now) {
		t = t.AddDate(0, 0, -7)
	}
	return t
}

// dashboardLink is where the rest is, or "" when Thicket doesn't know its
// address.
func (e *Engine) dashboardLink() string {
	if e.Cfg == nil {
		return ""
	}
	return strings.TrimRight(e.Cfg.WebBaseURL(), "/") + "/dashboard"
}

// WeeklyDigests sends each adult their weekly digest once its time has
// come in their timezone, at most once a week. The initiative loop runs
// it every few minutes.
func (e *Engine) WeeklyDigests(ctx context.Context) {
	day, minute, on := e.digestSchedule(ctx)
	if !on {
		return
	}
	host, ok := e.Chars.Host()
	if !ok || e.paused(ctx, host.ID) != nil {
		return
	}
	users, err := e.DB.ListUsers(ctx)
	if err != nil {
		e.Log.Error("weekly digest: list users", "err", err)
		return
	}
	now := e.now()
	for i := range users {
		u := &users[i]
		if u.IsKid() {
			continue
		}
		due := lastDigestTime(now, u.Location(), day, minute)
		if now.Sub(due) >= digestWindow || u.CreatedAt.After(due) {
			continue
		}
		mode, err := e.DB.DigestMode(ctx, u.ID)
		if err != nil || mode == store.DigestOff {
			continue
		}
		if e.quiet(host, u) {
			continue // after the host's quiet hours
		}
		key := fmt.Sprintf("digest:%d:%s", u.ID, due.Format("2006-01-02"))
		if claimed, err := e.DB.ClaimOnce(ctx, key); err != nil || !claimed {
			continue
		}
		e.sendDigest(ctx, u, host, due.AddDate(0, 0, -7), due, mode)
	}
}

// sendDigest builds and sends one digest. Its claim is already taken, so
// a failure goes to the inbox rather than being retried.
func (e *Engine) sendDigest(ctx context.Context, u *store.User, host *character.Character, from, to time.Time, mode string) {
	d, err := e.BuildDigest(ctx, u, from, to)
	if err != nil {
		e.Log.Error("build weekly digest", "user", u.ID, "err", err)
		e.logActivity(ctx, "error", "initiative", host.ID, u.ID, "the weekly digest couldn't be put together", err.Error())
		return
	}
	if d.Empty() && mode == store.DigestSkipEmpty {
		e.logActivity(ctx, "info", "initiative", host.ID, u.ID, "weekly digest: a quiet week, not sent (as they asked)", "")
		return
	}
	text := d.Text(e.dashboardLink())
	// Proactive, but outside the daily budget: sent directly, not counted.
	if err := e.sendToUser(ctx, u, host, text); err != nil {
		e.Log.Error("send weekly digest", "user", u.ID, "err", err)
		e.deadLetter(ctx, store.InboxItem{UserID: u.ID, Character: host.ID, Kind: store.InboxUndelivered,
			Ref: "digest:" + to.Format("2006-01-02"), Title: "Your weekly digest couldn't be delivered", Detail: text})
		return
	}
	e.logActivity(ctx, "info", "initiative", host.ID, u.ID, "sent the weekly digest", "")
}

// --- the weekly_digest tool ---

func (e *Engine) digestTools() []*Tool {
	return []*Tool{{
		Name: "weekly_digest",
		Description: "The weekly digest the user gets from you (what each of their sprigs did, what's waiting on them, what it cost). " +
			"show: build this past week's digest now and return it (relay it as it is: it's built from the records, don't change the numbers). " +
			"always / skip_empty / off: how they want it (skip_empty sends nothing in a quiet week; always sends one line). " +
			"The day and time are the household settings digest.day and digest.time, in their own timezone.",
		Params:   `{"type":"object","properties":{"action":{"type":"string","enum":["show","always","skip_empty","off"]}},"required":["action"]}`,
		Scope:    "host",
		ChatOnly: true,
		Run:      e.toolWeeklyDigest,
	}}
}

func (e *Engine) toolWeeklyDigest(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Action string `json:"action"`
	}](args)
	if err != nil {
		return "", err
	}
	if t.user.IsKid() {
		return "", fmt.Errorf("the weekly digest is for adults")
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "show" {
		to := e.now()
		d, err := e.BuildDigest(ctx, t.user, to.AddDate(0, 0, -7), to)
		if err != nil {
			return "", err
		}
		return d.Text(e.dashboardLink()), nil
	}
	if !store.ValidDigestMode(action) {
		return "", fmt.Errorf("action is show, always, skip_empty or off, not %q", a.Action)
	}
	if err := e.DB.SetDigestMode(ctx, t.user.ID, action); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "weekly digest: "+action, "")
	when := "off"
	if day, minute, on := e.digestSchedule(ctx); on {
		when = fmt.Sprintf("%ss at %02d:%02d their time", day, minute/60, minute%60)
	}
	switch action {
	case store.DigestOff:
		return "Done: no weekly digest for them any more.", nil
	case store.DigestSkipEmpty:
		return "Done: they get the weekly digest only in weeks with something in it (household schedule: " + when + ").", nil
	}
	return "Done: they get the weekly digest every week, one line in a quiet one (household schedule: " + when + ").", nil
}
