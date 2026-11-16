// Package dashboard serves a small read-only web page: your sprigs,
// memories, jobs and skills, and for admins spending, health and the
// activity log. Sign-in is by one-time link (from Juni, or
// `thicket dashboard-link`), exchanged for a session cookie.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

const (
	LoginTTL   = 10 * time.Minute
	sessionTTL = 7 * 24 * time.Hour
	cookieName = "thicket_session"
	loginPath  = "/dashboard/login"
)

func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// LoginLink makes a one-time sign-in link for a user. base is the public
// address (THICKET_PUBLIC_URL), or "" for a localhost one.
func LoginLink(ctx context.Context, db *store.Store, userID int64, base, httpAddr string) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if err := db.AddWebSession(ctx, hash, userID, "login", LoginTTL); err != nil {
		return "", err
	}
	if base == "" {
		port := "8080"
		if i := strings.LastIndex(httpAddr, ":"); i >= 0 && i < len(httpAddr)-1 {
			port = httpAddr[i+1:]
		}
		base = "http://localhost:" + port
	}
	return strings.TrimRight(base, "/") + loginPath + "?t=" + token, nil
}

// HealthFunc reports named checks (the same ones /health shows).
type HealthFunc func(ctx context.Context) map[string]Check

type Check struct {
	OK     bool
	Detail string
}

type Dashboard struct {
	DB       *store.Store
	Chars    *character.Registry
	Settings *settings.Settings
	Health   HealthFunc
	Version  string
	Log      *slog.Logger
	// Secure marks the cookie Secure (the dashboard is reached over https).
	Secure bool
	Now    func() time.Time
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
	mux.HandleFunc("GET /dashboard/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/dashboard", http.StatusSeeOther) })
	mux.HandleFunc("GET "+loginPath, d.login)
	mux.HandleFunc("POST /dashboard/logout", d.logout)
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (d *Dashboard) login(w http.ResponseWriter, r *http.Request) {
	uid, err := d.DB.UseWebLogin(r.Context(), hashToken(r.URL.Query().Get("t")))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, map[string]string{"Problem": "That sign-in link has expired or was already used. Ask Juni for a new one."})
		return
	}
	token, hash, err := newToken()
	if err == nil {
		err = d.DB.AddWebSession(r.Context(), hash, uid, "session", sessionTTL)
	}
	if err != nil {
		d.Log.Error("dashboard session", "err", err)
		http.Error(w, "couldn't sign you in", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/dashboard", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: d.Secure || r.TLS != nil, SameSite: http.SameSiteStrictMode})
	// Redirect so the link's token leaves the address bar and history.
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (d *Dashboard) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = d.DB.DeleteWebSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/dashboard", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = signInTmpl.Execute(w, map[string]string{"Problem": "You're signed out."})
}

func (d *Dashboard) user(r *http.Request) (*store.User, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, err
	}
	uid, err := d.DB.WebSessionUser(r.Context(), hashToken(c.Value), "session")
	if err != nil {
		return nil, err
	}
	return d.DB.UserByID(r.Context(), uid)
}

func (d *Dashboard) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	u, err := d.user(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, map[string]string{})
		return
	}
	data, err := d.gather(r.Context(), u)
	if err != nil {
		d.Log.Error("dashboard", "err", err)
		http.Error(w, "couldn't load the dashboard", http.StatusInternalServerError)
		return
	}
	if err := pageTmpl.Execute(w, data); err != nil {
		d.Log.Error("dashboard render", "err", err)
	}
}

// --- what the page shows ---

type pageData struct {
	User     *store.User
	Name     string
	Admin    bool
	Version  string
	Now      string
	Model    string
	Sprigs   []sprigView
	MemGroup []memGroup
	MemCount int
	Jobs     []jobView
	Skills   []store.Skill
	Spend    *spendView
	Health   []checkView
	Activity []activityView
}

type sprigView struct {
	Name, Role string
	Host       bool
	Scopes     []string
}

type memGroup struct {
	Title    string
	Memories []memView
}

type memView struct {
	Kind, Content, Due string
	Locked             bool
}

type jobView struct {
	ID                int64
	Name, Sprig, When string
	Enabled           bool
	Runs              []runView
}

type runView struct {
	At, Status, Error string
	Cost              string
}

type spendView struct {
	Today, Month         string
	DailyCap, MonthlyCap string
	BySprig              []barView
	ByDay                []barView
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
	At, Level, Summary string
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
	if url := d.Settings.String(ctx, "model_url"); !strings.Contains(url, "openrouter.ai") {
		p.Model += " on " + url
	} else {
		p.Model += " (OpenRouter)"
	}

	names := map[string]string{}
	for _, c := range d.Chars.All() {
		names[c.ID] = c.DisplayName()
		p.Sprigs = append(p.Sprigs, sprigView{Name: c.DisplayName(), Role: c.Role, Host: c.Host, Scopes: c.Tools})
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}

	mems, err := d.DB.AllMemories(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	groups := map[string][]memView{}
	for _, m := range mems {
		if !m.ExpiresAt.IsZero() && m.ExpiresAt.Before(d.now()) {
			continue
		}
		v := memView{Kind: m.Kind, Content: m.Content, Locked: m.Locked}
		if !m.DueAt.IsZero() {
			v.Due = m.DueAt.In(loc).Format("Mon 2 Jan")
		}
		key := "Shared with every sprig"
		if m.Character != "" {
			key = "Just with " + name(m.Character)
		}
		groups[key] = append(groups[key], v)
		p.MemCount++
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // shared first
		return keys[i] == "Shared with every sprig" || (keys[j] != "Shared with every sprig" && keys[i] < keys[j])
	})
	for _, k := range keys {
		p.MemGroup = append(p.MemGroup, memGroup{Title: k, Memories: groups[k]})
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

	if p.Skills, err = d.DB.Skills(ctx); err != nil {
		return nil, err
	}
	if !p.Admin {
		return p, nil
	}
	if p.Spend, err = d.spend(ctx); err != nil {
		return nil, err
	}
	if d.Health != nil {
		for n, c := range d.Health(ctx) {
			p.Health = append(p.Health, checkView{Name: n, OK: c.OK, Detail: c.Detail})
		}
		sort.Slice(p.Health, func(i, j int) bool { return p.Health[i].Name < p.Health[j].Name })
	}
	acts, err := d.DB.RecentActivity(ctx, 50, "info")
	if err != nil {
		return nil, err
	}
	for _, a := range acts {
		p.Activity = append(p.Activity, activityView{At: stamp(a.At), Level: a.Level, Summary: a.Summary})
	}
	return p, nil
}

func (d *Dashboard) spend(ctx context.Context) (*spendView, error) {
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
		} else if label == "" {
			label = "Background (memory review, summaries)"
		}
		s.BySprig = append(s.BySprig, barView{Label: label, Value: money(r.CostUSD), Pct: pct(r.CostUSD, top)})
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
