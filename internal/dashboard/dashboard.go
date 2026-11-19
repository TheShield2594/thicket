// Package dashboard serves a small, read-mostly web page: the sprig roster
// with live avatars, the "needs you" inbox (approve or deny from here),
// the trail of what sprigs did, memories, jobs and skills, and for admins
// costs and health. It has no sign-in of its own: it uses the web app's
// session (sign in there, then open /dashboard).
package dashboard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// SessionCookie is the web app's session cookie (path "/").
const SessionCookie = "thicket_session"

// HealthFunc reports named checks (the same ones /health shows).
type HealthFunc func(ctx context.Context) map[string]Check

type Check struct {
	OK     bool
	Detail string
}

type Dashboard struct {
	// States reports each sprig's live state (idle, thinking, ...) by id.
	States func(ctx context.Context) map[string]string
	// Decide answers a pending approval; RunDraft has a draft done.
	Decide   func(ctx context.Context, u *store.User, id int64, approve bool) error
	RunDraft func(ctx context.Context, u *store.User, id int64) error
	// Undo reverses a trail entry (permission changes, sprig calendar entries).
	Undo func(ctx context.Context, u *store.User, id int64) (string, error)
	// Pause pauses (on) or resumes everything (sprig "") or one sprig; it
	// checks who may.
	Pause func(ctx context.Context, u *store.User, sprig string, on bool) error

	DB       *store.Store
	Chars    *character.Registry
	Settings *settings.Settings
	Health   HealthFunc
	Version  string
	Log      *slog.Logger
	// SessionTTL is the web app's session lifetime: seeing the dashboard
	// keeps the session alive, as using the app does.
	SessionTTL time.Duration
	Now        func() time.Time
	// BaseURL is where people open Thicket, for invite links.
	BaseURL string
	// Device describes a browser from its user agent.
	Device func(ua string) string
	// Memories is the memory screen's engine side.
	Memories MemoryKeeper
}

func (d *Dashboard) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard", d.page)
	mux.HandleFunc("GET /dashboard/state.json", d.state)
	mux.HandleFunc("POST /dashboard/act", d.act)
	mux.HandleFunc("GET /dashboard/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/dashboard", http.StatusSeeOther) })
	return secureHeaders(mux)
}

type nonceKey struct{}

// secureHeaders sets a strict policy: no scripts but the page's own small
// one (by nonce), which only fetches live sprig states.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nb := make([]byte, 16)
		_, _ = rand.Read(nb)
		nonce := base64.RawStdEncoding.EncodeToString(nb)
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; connect-src 'self'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nonceKey{}, nonce)))
	})
}

// csrfToken ties a form to the session it was rendered for: forms can't
// send custom headers, so writes carry this instead.
func csrfToken(session string) string {
	m := hmac.New(sha256.New, []byte(session))
	m.Write([]byte("thicket-dashboard-act"))
	return hex.EncodeToString(m.Sum(nil))
}

func (d *Dashboard) user(r *http.Request) (*store.User, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, err
	}
	return d.DB.WebSessionUser(r.Context(), c.Value, d.SessionTTL)
}

func (d *Dashboard) page(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, http.StatusOK, flashFor(r.URL.Query().Get("ok")), "")
}

func flashFor(code string) string {
	switch code {
	case "approved":
		return "Approved. The sprig is on it and will report back in chat."
	case "denied":
		return "Denied. The sprig has been told."
	case "done":
		return "Done. The sprig will tell you how it went in chat."
	case "resolved":
		return "Cleared from your inbox."
	case "revoked":
		return "Signed out. That browser needs to sign in again."
	case "memory-saved":
		return "Saved. Sprigs use it from their next reply; Undo puts it back."
	case "memory-added":
		return "Added. Sprigs use it from their next reply."
	case "memory-deleted":
		return "Forgotten for good."
	case "memory-undone":
		return "Undone."
	}
	return ""
}

func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, code int, flash, problem string) {
	d.renderWith(w, r, code, func(p *pageData) { p.Flash, p.Problem = flash, problem })
}

func (d *Dashboard) renderWith(w http.ResponseWriter, r *http.Request, code int, set func(*pageData)) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	u, err := d.user(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, nil)
		return
	}
	data, err := d.gather(r.Context(), u)
	if err != nil {
		d.Log.Error("dashboard", "err", err)
		http.Error(w, "couldn't load the dashboard", http.StatusInternalServerError)
		return
	}
	c, _ := r.Cookie(SessionCookie)
	data.CSRF = csrfToken(c.Value)
	data.Nonce, _ = r.Context().Value(nonceKey{}).(string)
	if err := d.sessions(r.Context(), u, c.Value, data); err != nil {
		d.Log.Error("dashboard sessions", "err", err)
	}
	set(data)
	w.WriteHeader(code)
	if err := pageTmpl.Execute(w, data); err != nil {
		d.Log.Error("dashboard render", "err", err)
	}
}

// state is the roster's live states, polled by the page.
func (d *Dashboard) state(w http.ResponseWriter, r *http.Request) {
	u, err := d.user(r)
	if err != nil {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	st := map[string]string{}
	if d.States != nil {
		for id, s := range d.States(r.Context()) {
			// Only sprigs the page shows by id: never someone else's
			// private sprig (it would give its id away).
			c, ok := d.Chars.Get(id)
			if !ok || c.PrivateFrom(u.ID) || (!u.IsAdmin() && !c.UsableBy(u)) {
				continue
			}
			st[id] = s
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// act answers the inbox: approve or deny a request, have a draft done, or
// clear an item. Only the person it's for can.
func (d *Dashboard) act(w http.ResponseWriter, r *http.Request) {
	u, err := d.user(r)
	if err != nil {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	c, _ := r.Cookie(SessionCookie)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(csrfToken(c.Value))) {
		http.Error(w, "this form has expired; reload the dashboard", http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	ctx := r.Context()
	var ok, anchor string
	anchor = "inbox"
	switch kind, action := r.PostFormValue("kind"), r.PostFormValue("action"); {
	case kind == "session" && action == "revoke":
		owner := u.ID
		if u.IsAdmin() {
			owner = 0 // admins can sign anyone out
		}
		var done bool
		done, err = d.DB.RevokeWebSession(ctx, r.PostFormValue("sid"), owner)
		if err == nil && !done {
			err = errors.New("that session had already ended")
		}
		ok, anchor = "revoked", "signin"
	case kind == "invite" && action == "create" && u.IsAdmin():
		d.invite(w, r, u)
		return
	case kind == "approval" && (action == "approve" || action == "deny") && d.Decide != nil:
		err = d.Decide(ctx, u, id, action == "approve")
		ok = map[bool]string{true: "approved", false: "denied"}[action == "approve"]
	case kind == "pause" && (action == "pause" || action == "resume") && d.Pause != nil:
		err = d.Pause(ctx, u, r.PostFormValue("sprig"), action == "pause")
		ok, anchor = map[bool]string{true: "paused", false: "resumed"}[action == "pause"], "sprigs"
	case kind == "trail" && action == "undo" && d.Undo != nil:
		_, err = d.Undo(ctx, u, id)
		ok, anchor = "undone", "changes"
	case kind == "memory" && d.Memories != nil:
		ok, err = d.memoryAct(ctx, u, action, id, r)
		anchor = "memories"
	case kind == "inbox" && action == "run" && d.RunDraft != nil:
		err, ok = d.RunDraft(ctx, u, id), "done"
	case kind == "inbox" && action == "resolve":
		err, ok = d.DB.ResolveInbox(ctx, id, u.ID), "resolved"
		if errors.Is(err, store.ErrNotFound) {
			err = errors.New("that item was already cleared")
		}
	default:
		err = errors.New("unknown action")
	}
	if err != nil {
		d.render(w, r, http.StatusBadRequest, "", "Couldn't do that: "+err.Error())
		return
	}
	http.Redirect(w, r, "/dashboard?ok="+ok+"#"+anchor, http.StatusSeeOther)
}

// invite makes an invite link and shows it once (never in a URL).
func (d *Dashboard) invite(w http.ResponseWriter, r *http.Request, u *store.User) {
	role := "member"
	if r.PostFormValue("role") == "admin" {
		role = "admin"
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if len(name) > 60 {
		name = name[:60]
	}
	days, _ := strconv.Atoi(r.PostFormValue("days"))
	if days < 1 || days > 30 {
		days = 3
	}
	token, err := d.DB.CreateInvite(r.Context(), store.Invite{Role: role, Name: name, CreatedBy: u.ID}, time.Duration(days)*24*time.Hour)
	if err != nil {
		d.render(w, r, http.StatusInternalServerError, "", "Couldn't make an invite: "+err.Error())
		return
	}
	_ = d.DB.LogActivity(r.Context(), store.Activity{Level: "info", Kind: "system", UserID: u.ID, Summary: "made an invite link (" + role + ")"})
	d.renderWith(w, r, http.StatusOK, func(p *pageData) {
		p.InviteLink = strings.TrimRight(d.BaseURL, "/") + "/invite#" + token
		p.InviteDays = days
	})
}

// --- what the page shows ---

type pageData struct {
	CSRF, Nonce    string
	Flash, Problem string
	AvatarCSS      template.CSS
	Approvals      []approvalView
	Inbox          []inboxView
	Trail          []activityView
	Changes        []changeView
	Costs          []costRow
	Models         []costRow
	User           *store.User
	Name           string
	Admin          bool
	Version        string
	Now            string
	Model          string
	Sprigs         []sprigView
	MemBlocks      []memBlock
	MemCount       int
	MemKinds       []string
	Jobs           []jobView
	Tasks          []taskView
	Skills         []store.Skill
	Spend          *spendView
	Health         []checkView
	// Sign-in: your sessions, everyone's (admins), and an invite just made.
	Sessions, AllSessions []sessionRow
	// The kill switch: whether everything is paused, and whether this
	// person may pause or resume everything (the owner).
	PausedAll, CanPauseAll bool
	InviteLink             string
	InviteDays             int
}

// changeView is one entry of the reversible trail.
type changeView struct {
	ID               int64
	At, Kind, Sprig  string
	Summary, Detail  string
	Undoable, Undone bool
}

type sessionRow struct {
	ID, Who, Device, Method, Last, Started string
	Current                                bool
}

// sessions fills in the sign-in section.
func (d *Dashboard) sessions(ctx context.Context, u *store.User, current string, p *pageData) error {
	loc := u.Location()
	rows := func(userID int64) ([]sessionRow, error) {
		list, err := d.DB.WebSessions(ctx, userID, current)
		if err != nil {
			return nil, err
		}
		var out []sessionRow
		for _, s := range list {
			dev := s.UserAgent
			if d.Device != nil {
				dev = d.Device(s.UserAgent)
			}
			out = append(out, sessionRow{ID: s.PublicID, Who: s.UserName, Device: dev, Method: s.Method,
				Last: s.LastSeenAt.In(loc).Format("Mon 2 Jan 15:04"), Started: s.CreatedAt.In(loc).Format("2 Jan 2006"), Current: s.Current})
		}
		return out, nil
	}
	var err error
	if p.Sessions, err = rows(u.ID); err != nil {
		return err
	}
	if u.IsAdmin() {
		p.AllSessions, err = rows(0)
	}
	return err
}

type sprigView struct {
	ID, Name, Role string
	Only           string // whose sprig it is, if it isn't shared
	Host           bool
	// Private: someone else's solo sprig. Only its owner, state and
	// health are shown.
	Private bool
	Health  string
	Scopes  []string
	State   string
	Avatar  template.HTML
	// Proactive is the viewer's share of the sprig's daily budget of
	// unprompted messages and what's held for them ("" for kid sprigs).
	Proactive string
	// Paused: this sprig is paused. CanPause: the viewer is its owner.
	Paused, CanPause bool
}

type approvalView struct {
	ID              int64
	Sprig, Tool, At string
	Args            string
}

type inboxView struct {
	ID                         int64
	Kind, Title, Detail, Sprig string
	At                         string
	Count                      int
	Draft                      bool
}

type costRow struct {
	Label, Heartbeat, Task, Total, Calls string
}

type jobView struct {
	ID                int64
	Name, Sprig, When string
	Enabled           bool
	Runs              []runView
}

// taskView is a background task one sprig gave another (delegate_task).
type taskView struct {
	ID                       int64
	Sprig, From, Task        string
	Status, Error, At, Ended string
}

type runView struct {
	At, Status, Error string
	Cost              string
}

type sprigCapView struct {
	Label, Daily, Monthly, Today, Month string
	Over                                bool // at or past a cap
}

type spendView struct {
	Today, Month         string
	DailyCap, MonthlyCap string
	BySprig              []barView
	ByDay                []barView
	// Caps: sprigs with caps of their own, and their spend against them.
	Caps []sprigCapView
	// Estimates says how cost estimates compared with what things cost
	// this month ("" if none finished).
	Estimates string
	// Mix: how this month's chat spend split between earlier
	// conversation and kept PDF text ("" if nothing was measured).
	Mix                  string
	HistoryPct, DocPct   int
	HistorySet, DocChars string
}

type barView struct {
	Label, Value string
	Pct          int
}

type checkView struct {
	Name, Detail string
	OK           bool
}

type activityView struct {
	At, Level, Kind, Sprig, Summary string
}

func (d *Dashboard) gather(ctx context.Context, u *store.User) (*pageData, error) {
	loc := u.Location()
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(loc).Format("Mon 2 Jan 15:04")
	}
	p := &pageData{User: u, Name: u.Name, Admin: u.IsAdmin(), Version: d.Version, Now: stamp(d.now())}
	if p.Name == "" {
		p.Name = "you"
	}
	p.Model = d.Settings.String(ctx, "model")
	p.PausedAll, _ = d.DB.PausedExactly(ctx, store.PauseAll)
	p.CanPauseAll = u.Role == store.RoleOwner && d.Pause != nil
	if url := d.Settings.String(ctx, "model_url"); !strings.Contains(url, "openrouter.ai") {
		p.Model += " on " + url
	} else {
		p.Model += " (OpenRouter)"
	}

	names := map[string]string{}
	states := map[string]string{}
	if d.States != nil {
		states = d.States(ctx)
	}
	p.AvatarCSS = template.CSS(avatar.CSS)
	for _, c := range d.Chars.All() {
		names[c.ID] = c.DisplayName()
		// Members see shared sprigs and their own; admins see every sprig,
		// but of someone else's solo sprig only that it exists, whose it
		// is, how it's doing and (under Costs) what it costs.
		if !u.IsAdmin() && !c.UsableBy(u) {
			continue
		}
		if c.PrivateFrom(u.ID) {
			names[c.ID] = d.privateLabel(ctx, c)
			if !u.IsAdmin() {
				continue
			}
			st := states[c.ID]
			if st == "" {
				st = "idle"
			}
			v := sprigView{Name: names[c.ID], Private: true, State: st, Health: "working normally"}
			if n, err := d.DB.CharacterErrorsSince(ctx, c.ID, d.now().Add(-24*time.Hour)); err == nil && n > 0 {
				v.Health = fmt.Sprintf("%d error(s) in the last day", n)
			}
			p.Sprigs = append(p.Sprigs, v)
			continue
		}
		st := states[c.ID]
		if st == "" {
			st = "idle"
		}
		look := avatar.Load(d.Chars.Dir(), c.ID, c.Color)
		// Inline renders only validated parts and escapes the label.
		svg := template.HTML(avatar.Inline(look, st, "av-"+c.ID, c.DisplayName()+" is "+st))
		v := sprigView{ID: c.ID, Name: c.DisplayName(), Role: c.Role, Host: c.Host, Scopes: c.Tools, State: st, Avatar: svg}
		// Its own pause decides the button, even under a pause of everything;
		// either kind shows it as paused.
		v.Paused, _ = d.DB.PausedExactly(ctx, c.ID)
		if _, any, _ := d.DB.PausedFor(ctx, c.ID); any {
			v.State = "paused"
		}
		v.CanPause = !c.IsKid() && (c.BelongsTo == u.ID || c.BelongsTo == 0 && u.IsAdmin()) || c.IsKid() && u.IsAdmin()
		if !c.IsKid() && c.UsableBy(u) {
			sent, _ := d.DB.ProactiveSentSince(ctx, u.ID, c.ID, store.LocalDayStart(d.now(), u.Location()))
			v.Proactive = fmt.Sprintf("Unprompted messages to you today: %d of %d", sent, c.DailyBudget())
			if held, _ := d.DB.HeldMessages(ctx, u.ID, c.ID); len(held) > 0 {
				v.Proactive += fmt.Sprintf(" · %d held for later", len(held))
			}
			if c.Initiative.QuietHours != "" {
				v.Proactive += " · quiet " + c.Initiative.QuietHours
			}
		}
		if c.BelongsTo != 0 {
			v.Only = "yours"
			if c.BelongsTo != u.ID {
				v.Only = fmt.Sprintf("user %d's", c.BelongsTo)
				if o, err := d.DB.UserByID(ctx, c.BelongsTo); err == nil && o.Name != "" {
					v.Only = o.Name + "'s"
				}
			}
		}
		p.Sprigs = append(p.Sprigs, v)
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		if id != "" {
			return removedSprig // its file is gone: it may have been someone's private sprig
		}
		return id
	}

	if err := d.memories(ctx, u, p); err != nil {
		return nil, err
	}

	jobs, err := d.DB.UserJobs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		v := jobView{ID: j.ID, Name: j.Name, Sprig: name(j.Character), When: j.Schedule, Enabled: j.Enabled}
		if j.Kind == store.JobWebhook {
			v.When = "webhook"
		}
		runs, _ := d.DB.JobRuns(ctx, j.ID, 5)
		for _, r := range runs {
			rv := runView{At: stamp(r.StartedAt), Status: r.Status, Error: r.Error}
			if r.CostUSD > 0 {
				rv.Cost = money(r.CostUSD)
			}
			v.Runs = append(v.Runs, rv)
		}
		p.Jobs = append(p.Jobs, v)
	}

	tasks, err := d.DB.UserTasks(ctx, u.ID, 10)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		p.Tasks = append(p.Tasks, taskView{ID: t.ID, Sprig: name(t.Character), From: name(t.From), Task: t.Prompt,
			Status: t.Status, Error: t.Error, At: stamp(t.StartedAt), Ended: stamp(t.FinishedAt)})
	}

	if p.Skills, err = d.DB.Skills(ctx); err != nil {
		return nil, err
	}

	apps, err := d.DB.PendingApprovals(ctx, u.ID, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		what := a.Tool
		if strings.HasPrefix(what, "thicket:") {
			what = strings.ReplaceAll(strings.TrimPrefix(what, "thicket:"), "_", " ")
		}
		p.Approvals = append(p.Approvals, approvalView{ID: a.ID, Sprig: name(a.Character), Tool: what, At: stamp(a.CreatedAt), Args: prettyJSON(a.Args)})
	}
	items, err := d.DB.OpenInbox(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		v := inboxView{ID: it.ID, Kind: it.Kind, Title: it.Title, Detail: it.Detail, Sprig: name(it.Character),
			At: stamp(it.UpdatedAt), Count: it.Count}
		if it.Kind == store.InboxDraft {
			// "Do it" runs the stored action: show exactly that.
			if exact, ok := draftArgs(it.Action); ok {
				v.Detail, v.Draft = exact, true
			}
		}
		p.Inbox = append(p.Inbox, v)
	}
	var acts []store.Activity
	if p.Admin {
		acts, err = d.DB.RecentActivity(ctx, 60, "info")
	} else {
		acts, err = d.DB.UserActivity(ctx, u.ID, 60)
	}
	if err != nil {
		return nil, err
	}
	for _, a := range acts {
		if d.Chars.PrivateFrom(a.Character, u.ID) {
			continue // someone else's private sprig: none of its activity
		}
		p.Trail = append(p.Trail, activityView{At: stamp(a.At), Level: a.Level, Kind: a.Kind, Sprig: name(a.Character), Summary: a.Summary})
	}
	if !u.IsKid() {
		who := u.ID
		if p.Admin {
			who = 0
		}
		changes, err := d.DB.Trail(ctx, who, 50)
		if err != nil {
			return nil, err
		}
		for _, c := range changes {
			if d.Chars.PrivateFrom(c.Character, u.ID) {
				continue
			}
			p.Changes = append(p.Changes, changeView{ID: c.ID, At: stamp(c.At), Kind: c.Kind, Sprig: name(c.Character), Summary: c.Summary,
				Detail: c.Detail, Undoable: c.Undoable, Undone: !c.UndoneAt.IsZero()})
		}
	}
	if !p.Admin {
		return p, nil
	}
	if p.Costs, p.Models, err = d.costs(ctx, name); err != nil {
		return nil, err
	}
	if p.Spend, err = d.spend(ctx, u); err != nil {
		return nil, err
	}
	if d.Health != nil {
		for n, c := range d.Health(ctx) {
			p.Health = append(p.Health, checkView{Name: n, OK: c.OK, Detail: c.Detail})
		}
		sort.Slice(p.Health, func(i, j int) bool { return p.Health[i].Name < p.Health[j].Name })
	}
	return p, nil
}

// removedSprig labels a sprig id that no longer loads.
const removedSprig = "a removed sprig"

// privateLabel names someone's solo sprig without saying anything about
// it: "Kim's private sprig".
func (d *Dashboard) privateLabel(ctx context.Context, c *character.Character) string {
	if o, err := d.DB.UserByID(ctx, c.BelongsTo); err == nil && o.Name != "" {
		return o.Name + "'s private sprig"
	}
	return fmt.Sprintf("user %d's private sprig", c.BelongsTo)
}

// costs is this month's spend per sprig, split heartbeat/task, and per model.
func (d *Dashboard) costs(ctx context.Context, name func(string) string) ([]costRow, []costRow, error) {
	now := d.now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := d.DB.UsageSummary(ctx, month, "character", "origin")
	if err != nil {
		return nil, nil, err
	}
	type split struct{ hb, task float64 }
	by := map[string]*split{}
	var order []string
	for _, r := range rows {
		k := r.Keys[0]
		if by[k] == nil {
			by[k] = &split{}
			order = append(order, k)
		}
		if r.Keys[1] == "heartbeat" {
			by[k].hb += r.CostUSD
		} else {
			by[k].task += r.CostUSD
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]].hb+by[order[i]].task > by[order[j]].hb+by[order[j]].task })
	var sprigs []costRow
	for _, k := range order {
		label := name(k)
		if k == "" {
			label = "Background (memory review, summaries)"
		}
		sprigs = append(sprigs, costRow{Label: label, Heartbeat: money(by[k].hb), Task: money(by[k].task), Total: money(by[k].hb + by[k].task)})
	}
	mrows, err := d.DB.UsageSummary(ctx, month, "model")
	if err != nil {
		return nil, nil, err
	}
	var models []costRow
	for _, r := range mrows {
		models = append(models, costRow{Label: r.Keys[0], Total: money(r.CostUSD), Calls: strconv.Itoa(r.Calls)})
	}
	return sprigs, models, nil
}

// prettyJSON shows arguments in full (the box scrolls) and exactly as
// stored: indented, never re-encoded, so a person sees what will run.
func prettyJSON(s string) string {
	var b bytes.Buffer
	if json.Indent(&b, []byte(s), "", "  ") == nil {
		return b.String()
	}
	return s
}

// draftArgs is a draft's exact call, from its stored action (its detail
// text is only a description).
func draftArgs(action string) (string, bool) {
	var a struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal([]byte(action), &a) != nil || a.Tool == "" {
		return "", false
	}
	return a.Tool + "\n" + prettyJSON(string(a.Args)), true
}

func (d *Dashboard) spend(ctx context.Context, u *store.User) (*spendView, error) {
	now := d.now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	today, err := d.DB.SpendSince(ctx, day, "")
	if err != nil {
		return nil, err
	}
	mtd, err := d.DB.SpendSince(ctx, month, "")
	if err != nil {
		return nil, err
	}
	s := &spendView{Today: money(today), Month: money(mtd), DailyCap: capOf(d.Settings.Float(ctx, "budget.daily_usd")), MonthlyCap: capOf(d.Settings.Float(ctx, "budget.monthly_usd"))}
	rows, err := d.DB.SpendBreakdown(ctx, month, "character")
	if err != nil {
		return nil, err
	}
	top := 0.0
	for _, r := range rows {
		top = max(top, r.CostUSD)
	}
	for _, r := range rows {
		label := r.Key
		if c, ok := d.Chars.Get(r.Key); ok {
			label = c.DisplayName()
			if c.PrivateFrom(u.ID) {
				label = d.privateLabel(ctx, c)
			}
		} else if label != "" {
			label = removedSprig
		} else {
			label = "Background (memory review, summaries)"
		}
		s.BySprig = append(s.BySprig, barView{Label: label, Value: money(r.CostUSD), Pct: pct(r.CostUSD, top)})
	}
	for _, c := range d.Chars.All() {
		dc, mc := d.Settings.Float(ctx, settings.SprigCap(c.ID, "daily")), d.Settings.Float(ctx, settings.SprigCap(c.ID, "monthly"))
		if dc <= 0 && mc <= 0 {
			continue
		}
		st, err := d.DB.SpendSince(ctx, day, c.ID)
		if err != nil {
			return nil, err
		}
		sm, err := d.DB.SpendSince(ctx, month, c.ID)
		if err != nil {
			return nil, err
		}
		label := c.DisplayName()
		if c.PrivateFrom(u.ID) {
			label = d.privateLabel(ctx, c)
		}
		s.Caps = append(s.Caps, sprigCapView{Label: label, Daily: capOf(dc), Monthly: capOf(mc), Today: money(st), Month: money(sm),
			Over: dc > 0 && st >= dc || mc > 0 && sm >= mc})
	}
	if n, est, act, err := d.DB.EstimateAccuracy(ctx, month); err == nil && n > 0 {
		s.Estimates = budget.EstimateLine(n, est, act)
	}
	mix, err := d.DB.PromptMixSince(ctx, month)
	if err != nil {
		return nil, err
	}
	if s.Mix = budget.MixLine(mix); s.Mix != "" {
		s.HistoryPct, s.DocPct = int(100*mix.HistoryShare()+0.5), int(100*mix.DocShare()+0.5)
		s.HistorySet = "automatic"
		if n := d.Settings.Int(ctx, "history_tokens"); n > 0 {
			s.HistorySet = fmt.Sprintf("%d tokens", n)
		}
		s.DocChars = fmt.Sprintf("%d characters", d.Settings.Int(ctx, "document_chars"))
	}
	days, err := d.DB.SpendByDay(ctx, day.AddDate(0, 0, -13))
	if err != nil {
		return nil, err
	}
	byDay := map[string]float64{}
	top = 0
	for _, x := range days {
		byDay[x.Day.Format("2006-01-02")] = x.CostUSD
		top = max(top, x.CostUSD)
	}
	for i := 13; i >= 0; i-- {
		dt := day.AddDate(0, 0, -i)
		c := byDay[dt.Format("2006-01-02")]
		s.ByDay = append(s.ByDay, barView{Label: dt.Format("Mon 2 Jan"), Value: money(c), Pct: pct(c, top)})
	}
	return s, nil
}

func money(v float64) string {
	if v > 0 && v < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

func capOf(v float64) string {
	if v <= 0 {
		return "no cap"
	}
	return "cap " + money(v)
}

func pct(v, top float64) int {
	if top <= 0 {
		return 0
	}
	return int(v / top * 100)
}
