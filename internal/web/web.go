// Package web is Thicket's browser app: a chat channel adapter plus the
// small JSON API and event stream the single-page app talks to.
//
// Each sprig gets its own thread per user ("<user id>.<character id>"),
// and "<user id>.all" is a shared room where any sprig can be addressed by
// name, like a Discord DM. Sign-in is by username and password, single
// sign-on (OIDC), an invite, or a one-time link: from the CLI (`thicket web
// link`), the startup log on a fresh install, or a sprig in a Discord DM
// (see accounts.go). Each person sees the shared sprigs and their own.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

//go:embed static
var staticFS embed.FS

const (
	ChannelName   = "web"
	sessionCookie = "thicket_session"
	SessionTTL    = 30 * 24 * time.Hour
	maxTextLen    = 8000
	roomID        = "all"
)

// Event is pushed to a signed-in browser over the event stream.
type Event struct {
	Type    string            `json:"type"` // message | typing | deleted | sprigs | audio
	Chat    string            `json:"chat,omitempty"`
	Message *store.WebMessage `json:"message,omitempty"`
	ID      int64             `json:"id,omitempty"`
}

// Status is what the app shows about the rest of Thicket.
type Status struct {
	Channels  map[string]bool `json:"channels"`
	ModelKey  bool            `json:"model_key"`
	SpendDay  float64         `json:"spend_day"`
	SpendMon  float64         `json:"spend_month"`
	BudgetDay float64         `json:"budget_day"`
	BudgetMon float64         `json:"budget_month"`
}

type Adapter struct {
	DB    *store.Store
	Chars *character.Registry
	Log   *slog.Logger
	// Status reports health for the app header (admins see spend too).
	Status func(ctx context.Context, admin bool) Status
	// SecureCookies forces the Secure flag (set when PublicURL is https).
	SecureCookies bool
	// Pin pins or unpins the conversation message behind a web message,
	// returning what to tell the user (the engine's PinWebMessage).
	Pin func(ctx context.Context, userID int64, chatRef, platformID string, pin bool) (string, error)
	// Remember keeps the conversation message behind a web message as a
	// locked memory, returning what the sprig said (the engine's
	// RememberWebMessage).
	Remember func(ctx context.Context, userID int64, chatRef, platformID string) (string, error)
	// OIDC is the single sign-on provider, if one is configured.
	OIDC *OIDCLogin
	// For API v2: the engine's household operations, the calendar, kid
	// sprig creation, and the address invites link to.
	Household Household
	// Admin backs the Settings screen (owners and admins): models and
	// other settings, and connectors.
	Admin        Admin
	Calendar     *calendar.Service
	MakeKidSprig KidSprigMaker
	BaseURL      string
	// Joined is called when someone new accepts an invite: the engine's
	// WebJoined, which greets them in the background.
	Joined func(ctx context.Context, u *store.User)
	// PingEvery is how often an open event stream is kept alive and its
	// session re-checked (default 25s).
	PingEvery time.Duration

	onDelete func(ctx context.Context, d channel.Deletion)

	running atomic.Bool
	mu      sync.RWMutex
	handler channel.Handler
	runCtx  context.Context
	subs    map[int64]map[chan Event]struct{}

	// stopped closes when Run returns. It exists from New, so a stream that
	// opens before Run starts still sees shutdown.
	stopped  chan struct{}
	stopOnce sync.Once

	users        userThrottle
	loginMu      sync.Mutex
	loginFails   int // failed redemptions in this window
	loginPending int // admitted attempts still being redeemed
	loginStart   time.Time
}

// Failed sign-ins are limited globally: tokens are unguessable, so this
// only stops a flood of bad requests from tying up the database.
const (
	loginFailLimit  = 30
	loginFailWindow = time.Minute
)

// loginBegin admits a sign-in attempt to the database, reserving a slot
// so a concurrent burst can't all pass before failures are counted. Every
// admitted attempt must call loginEnd.
func (a *Adapter) loginBegin(now time.Time) bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if now.Sub(a.loginStart) >= loginFailWindow {
		a.loginStart, a.loginFails = now, 0
	}
	if a.loginFails+a.loginPending >= loginFailLimit {
		return false
	}
	a.loginPending++
	return true
}

// loginEnd releases an admitted attempt's slot, counting it if it failed.
func (a *Adapter) loginEnd(failed bool) {
	a.loginMu.Lock()
	a.loginPending--
	if failed {
		a.loginFails++
	}
	a.loginMu.Unlock()
}

func New(db *store.Store, chars *character.Registry, log *slog.Logger) *Adapter {
	return &Adapter{DB: db, Chars: chars, Log: log, subs: map[int64]map[chan Event]struct{}{}, stopped: make(chan struct{})}
}

// ---- channel.Channel ----

func (a *Adapter) Name() string    { return ChannelName }
func (a *Adapter) Connected() bool { return a.running.Load() }

func (a *Adapter) Run(ctx context.Context, h channel.Handler) error {
	a.mu.Lock()
	a.handler, a.runCtx = h, ctx
	a.mu.Unlock()
	a.running.Store(true)
	<-ctx.Done()
	a.running.Store(false)
	a.stopOnce.Do(func() { close(a.stopped) })
	return nil
}

// parseChat splits "<user id>.<sprig|all>".
func parseChat(chatID string) (int64, string, bool) {
	u, sprig, ok := strings.Cut(chatID, ".")
	if !ok || sprig == "" {
		return 0, "", false
	}
	uid, err := strconv.ParseInt(u, 10, 64)
	return uid, sprig, err == nil && uid > 0
}

func (a *Adapter) Send(ctx context.Context, chatID string, p channel.Persona, text string) error {
	_, err := a.SendIDs(ctx, chatID, p, text)
	return err
}

// SendIDs posts a sprig's message and returns its id.
func (a *Adapter) SendIDs(ctx context.Context, chatID string, p channel.Persona, text string) ([]string, error) {
	uid, _, ok := parseChat(chatID)
	if !ok {
		return nil, fmt.Errorf("web: bad chat id %q", chatID)
	}
	// The app names a speaker from the roster. Keep the name only when the
	// roster can't supply it: a preview of a sprig that doesn't exist yet.
	name := ""
	if c, ok := a.Chars.Get(p.ID); !ok || c.DisplayName() != p.Name {
		name = p.Name
	}
	m, err := a.DB.AddWebMessage(ctx, uid, store.WebMessage{ChatID: chatID, Role: "assistant", Character: p.ID, Name: name, Content: text})
	if err != nil {
		return nil, err
	}
	a.publish(uid, Event{Type: "message", Chat: chatID, Message: &m})
	return []string{strconv.FormatInt(m.ID, 10)}, nil
}

// RosterChanged tells every open app that the sprigs changed (one was
// created, edited or removed), so sidebars update without a reload.
func (a *Adapter) RosterChanged() {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, subs := range a.subs {
		for ch := range subs {
			select {
			case ch <- Event{Type: "sprigs"}:
			default:
			}
		}
	}
}

// OnDelete registers what to call when someone deletes a message in the
// app. Call it before Run.
func (a *Adapter) OnDelete(f func(ctx context.Context, d channel.Deletion)) { a.onDelete = f }

func (a *Adapter) SendDirect(ctx context.Context, userID string, p channel.Persona, text string) (string, error) {
	u, err := a.DB.UserByExternal(ctx, ChannelName, userID)
	if err != nil {
		return "", fmt.Errorf("web: user %s: %w", userID, err)
	}
	sprig := p.ID
	if sprig == "" {
		sprig = roomID
	}
	chatID := fmt.Sprintf("%d.%s", u.ID, sprig)
	return chatID, a.Send(ctx, chatID, p, text)
}

func (a *Adapter) Delete(ctx context.Context, chatID, messageID string) error {
	uid, _, ok := parseChat(chatID)
	id, err := strconv.ParseInt(messageID, 10, 64)
	if !ok || err != nil {
		return fmt.Errorf("web: bad message ref")
	}
	if done, err := a.DB.DeleteWebMessage(ctx, chatID, id); err != nil || !done {
		return err
	}
	a.publish(uid, Event{Type: "deleted", Chat: chatID, ID: id})
	return nil
}

func (a *Adapter) Typing(_ context.Context, chatID string) {
	if uid, _, ok := parseChat(chatID); ok {
		a.publish(uid, Event{Type: "typing", Chat: chatID})
	}
}

// ---- event fan-out ----

func (a *Adapter) subscribe(uid int64) chan Event {
	ch := make(chan Event, 64)
	a.mu.Lock()
	if a.subs[uid] == nil {
		a.subs[uid] = map[chan Event]struct{}{}
	}
	a.subs[uid][ch] = struct{}{}
	a.mu.Unlock()
	return ch
}

func (a *Adapter) unsubscribe(uid int64, ch chan Event) {
	a.mu.Lock()
	delete(a.subs[uid], ch)
	if len(a.subs[uid]) == 0 {
		delete(a.subs, uid)
	}
	a.mu.Unlock()
}

func (a *Adapter) publish(uid int64, ev Event) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for ch := range a.subs[uid] {
		select {
		case ch <- ev:
		default: // a stalled tab catches up on reload; never block a turn
		}
	}
}

// ---- HTTP ----

// Handler serves the app and its API. Mount it at "/".
func (a *Adapter) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	index := func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "app missing from build", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /login", index)
	mux.HandleFunc("GET /invite", index)
	mux.HandleFunc("GET /c/{chat}", index)
	mux.HandleFunc("GET /app/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/app", files).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Service-Worker-Allowed", "/")
		http.ServeFileFS(w, r, static, "sw.js")
	})
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, static, "manifest.webmanifest")
	})

	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("GET /api/auth", a.authOptions)
	mux.HandleFunc("POST /api/invite/check", a.inviteInfo)
	mux.HandleFunc("POST /api/invite", a.redeemInvite)
	mux.HandleFunc("POST /api/oidc/start", a.oidcStart)
	mux.HandleFunc("GET /auth/oidc/callback", a.oidcCallback)
	mux.HandleFunc("GET /api/account", a.auth(a.account))
	mux.HandleFunc("POST /api/account/password", a.auth(a.changePassword))
	mux.HandleFunc("POST /api/account/profile", a.auth(a.saveProfile))
	mux.HandleFunc("DELETE /api/account/sessions/{id}", a.auth(a.revokeSession))
	mux.HandleFunc("GET /api/settings", a.auth(a.adminOnly(a.getSettings)))
	mux.HandleFunc("POST /api/settings", a.auth(a.adminOnly(a.setSetting)))
	mux.HandleFunc("GET /api/integrations", a.auth(a.adminOnly(a.getIntegrations)))
	mux.HandleFunc("POST /api/integrations", a.auth(a.adminOnly(a.addIntegration)))
	mux.HandleFunc("DELETE /api/integrations/{name}", a.auth(a.adminOnly(a.removeIntegration)))
	mux.HandleFunc("POST /api/logout", a.auth(a.logout))
	mux.HandleFunc("GET /api/me", a.auth(a.me))
	mux.HandleFunc("GET /api/sprigs/{id}", a.auth(a.sprig))
	mux.HandleFunc("GET /api/chats/{chat}/messages", a.auth(a.messages))
	mux.HandleFunc("POST /api/chats/{chat}/messages", a.auth(a.post))
	mux.HandleFunc("DELETE /api/chats/{chat}/messages/{id}", a.auth(a.deleteMessage))
	mux.HandleFunc("POST /api/chats/{chat}/messages/{id}/pin", a.auth(a.pinMessage))
	mux.HandleFunc("POST /api/chats/{chat}/messages/{id}/remember", a.auth(a.rememberMessage))
	mux.HandleFunc("GET /api/chats/{chat}/messages/{id}/audio", a.auth(a.audio))
	mux.HandleFunc("PUT /api/sprigs/{id}/voice", a.auth(a.setVoice))
	mux.HandleFunc("GET /api/events", a.auth(a.events))
	// The calendar and kid chats screens (same handlers as API v2).
	mux.HandleFunc("GET /api/calendar", a.auth(a.v2Calendar))
	mux.HandleFunc("GET /api/calendars", a.auth(a.v2Calendars))
	mux.HandleFunc("POST /api/calendars", a.auth(a.v2AddCalendar))
	mux.HandleFunc("DELETE /api/calendars/{id}", a.auth(a.v2RemoveCalendar))
	mux.HandleFunc("DELETE /api/calendar/outputs/{id}", a.auth(a.v2RemoveOutput))
	mux.HandleFunc("GET /api/kid-sprigs/{id}/chats", a.auth(a.v2KidChats))
	mux.HandleFunc("PATCH /api/kid-sprigs/{id}/access", a.auth(a.v2KidAccess))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

type authed func(w http.ResponseWriter, r *http.Request, u *store.User)

// auth resolves the session cookie. Writes also need the X-Thicket
// header, which a cross-site form or image can't send (CSRF).
func (a *Adapter) auth(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Thicket") != "1" {
			writeErr(w, http.StatusForbidden, "missing X-Thicket header")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		u, err := a.DB.WebSessionUser(r.Context(), c.Value, SessionTTL)
		if errors.Is(err, store.ErrNotFound) {
			a.clearCookie(w, r)
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		next(w, r, u)
	}
}

func (a *Adapter) secure(r *http.Request) bool {
	return a.SecureCookies || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (a *Adapter) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *Adapter) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Thicket") != "1" {
		writeErr(w, http.StatusForbidden, "missing X-Thicket header")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || (body.Token == "" && body.Username == "") {
		writeErr(w, http.StatusBadRequest, "missing token")
		return
	}
	if body.Token == "" {
		a.passwordLogin(w, r, strings.TrimSpace(body.Username), body.Password)
		return
	}
	ctx := r.Context()
	if !a.loginBegin(time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	uid, err := a.DB.RedeemWebLink(ctx, body.Token)
	a.loginEnd(errors.Is(err, store.ErrNotFound))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "That link has expired or was already used. Ask for a new one.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if uid == 0 {
		u, err := a.ownerOrCreate(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "couldn't set up the owner account")
			return
		}
		uid = u.ID
	}
	if err := a.startSession(w, r, uid, "link"); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ownerOrCreate returns the owner, creating a web-only owner on a fresh
// install.
func (a *Adapter) ownerOrCreate(ctx context.Context) (*store.User, error) {
	if u, err := a.DB.Owner(ctx); err == nil {
		return u, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	ext, _ := store.NewToken()
	return a.DB.CreateUser(ctx, ChannelName, "w-"+ext[:16], "", "owner")
}

func (a *Adapter) logout(w http.ResponseWriter, r *http.Request, _ *store.User) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.DB.DeleteWebSession(r.Context(), c.Value)
	}
	a.clearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- views ----

type sprigView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	FullName    string   `json:"full_name"`
	Role        string   `json:"role"`
	Color       string   `json:"color"`
	Avatar      string   `json:"avatar,omitempty"`
	Host        bool     `json:"host"`
	Traits      []string `json:"traits"`
	Catchphrase string   `json:"catchphrase,omitempty"`
	Abilities   []string `json:"abilities"`
	Memories    int      `json:"memories"`
	CheckIns    bool     `json:"check_ins"`
	FollowUps   bool     `json:"follow_ups"`
	QuietHours  string   `json:"quiet_hours,omitempty"`
	// DailyMessages is the sprig's daily budget of unprompted messages per
	// person (0 for kid sprigs, which send none).
	DailyMessages int `json:"daily_messages,omitempty"`
	// Speaks is when the sprig answers out loud (never, when_spoken_to,
	// always); VoiceMode is how it speaks to this person (brief, full,
	// off).
	Speaks    string `json:"speaks"`
	VoiceMode string `json:"voice_mode"`
}

func (a *Adapter) avatarURL(c *character.Character) string {
	switch {
	case strings.HasPrefix(c.Avatar, "https://"):
		return c.Avatar
	case c.Avatar != "":
		if _, err := os.Stat(filepath.Join(a.Chars.Dir(), filepath.Base(c.Avatar))); err == nil {
			return "/avatars/" + filepath.Base(c.Avatar)
		}
	}
	return ""
}

func abilities(c *character.Character) []string {
	var out []string
	for _, t := range c.Tools {
		switch {
		case t == character.ScopeSchedule:
			out = append(out, "Schedules routines")
		case t == character.ScopeCharacters:
			out = append(out, "Brings on new sprigs")
		case t == character.ScopeAdmin:
			out = append(out, "Manages settings")
		case strings.HasPrefix(t, character.ScopeMCPPrefix):
			srv, _, _ := strings.Cut(strings.TrimPrefix(t, character.ScopeMCPPrefix), "/")
			out = append(out, "Uses "+srv)
		}
	}
	return out
}

func (a *Adapter) sprigViews(ctx context.Context, u *store.User) []sprigView {
	counts, _ := a.DB.CountMemories(ctx, u.ID)
	var out []sprigView
	for _, c := range a.Chars.For(u) { // shared sprigs and their own
		v := sprigView{ID: c.ID, Name: c.DisplayName(), FullName: c.Name, Role: c.Role, Color: c.Color, Avatar: a.avatarURL(c),
			Host: c.Host, Traits: c.Personality.Traits, Abilities: abilities(c), Memories: counts[c.ID],
			CheckIns: c.Initiative.CheckIns, FollowUps: c.Initiative.FollowUps, QuietHours: c.Initiative.QuietHours, Speaks: speaks(c)}
		if !c.IsKid() {
			v.DailyMessages = c.DailyBudget()
		}
		v.VoiceMode, _ = a.DB.SpeechMode(ctx, u.ID, c.ID)
		if v.Color == "" {
			v.Color = "#7c8cff"
		}
		if len(c.Personality.Catchphrases) > 0 {
			v.Catchphrase = c.Personality.Catchphrases[0]
		}
		if v.Traits == nil {
			v.Traits = []string{}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Host && !out[j].Host })
	return out
}

func (a *Adapter) me(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	latest, _ := a.DB.LatestWebMessages(ctx, u.ID)
	if latest == nil {
		latest = []store.WebMessage{}
	}
	counts, _ := a.DB.CountMemories(ctx, u.ID)
	var st Status
	if a.Status != nil {
		st = a.Status(ctx, u.IsAdmin())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":            map[string]any{"id": u.ID, "name": u.Name, "role": u.Role, "channel": u.Channel, "timezone": u.Timezone},
		"sprigs":          a.sprigViews(ctx, u),
		"latest":          latest,
		"shared_memories": counts[""],
		"status":          st,
		"parent_of":       a.parentOf(ctx, u),
		"calendar":        a.Calendar != nil && !u.IsKid(),
		"settings":        a.Admin != nil && u.IsAdmin(),
	})
}

type routineView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"`
	Enabled   bool      `json:"enabled"`
	LastRunAt time.Time `json:"last_run_at,omitzero"`
	LastState string    `json:"last_status,omitempty"`
}

func (a *Adapter) sprig(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	c, ok := a.Chars.Get(r.PathValue("id"))
	if !ok || !c.UsableBy(u) {
		writeErr(w, http.StatusNotFound, "no such sprig")
		return
	}
	jobs, err := a.DB.UserJobs(ctx, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	routines := []routineView{}
	for _, j := range jobs {
		if j.Character != c.ID {
			continue
		}
		v := routineView{ID: j.ID, Name: j.Name, Schedule: j.Schedule, Enabled: j.Enabled, LastRunAt: j.LastRunAt}
		if runs, err := a.DB.JobRuns(ctx, j.ID, 1); err == nil && len(runs) > 0 {
			v.LastState = runs[0].Status
		}
		routines = append(routines, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"routines": routines, "proactive": a.proactiveView(ctx, u, c)})
}

type heldView struct {
	Title  string    `json:"title"`
	Reason string    `json:"reason"` // quiet | budget
	At     time.Time `json:"at"`
}

// proactiveView is how much of the sprig's daily budget of unprompted
// messages u has had today, and what's held for them (nil for kid
// sprigs, which send none).
func (a *Adapter) proactiveView(ctx context.Context, u *store.User, c *character.Character) map[string]any {
	if c.IsKid() {
		return nil
	}
	sent, _ := a.DB.ProactiveSentSince(ctx, u.ID, c.ID, store.LocalDayStart(time.Now(), u.Location()))
	held := []heldView{}
	if hs, err := a.DB.HeldMessages(ctx, u.ID, c.ID); err == nil {
		for _, h := range hs {
			held = append(held, heldView{Title: h.Title, Reason: h.Reason, At: h.CreatedAt})
		}
	}
	return map[string]any{"daily_messages": c.DailyBudget(), "sent_today": sent, "held": held}
}

// chatFor checks that the chat belongs to u and names a real sprig.
func (a *Adapter) chatFor(u *store.User, chatID string) (string, bool) {
	uid, sprig, ok := parseChat(chatID)
	if !ok || uid != u.ID {
		return "", false
	}
	if sprig == roomID {
		return "", true
	}
	c, exists := a.Chars.Get(sprig)
	return sprig, exists && c.UsableBy(u)
}

func (a *Adapter) messages(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	if _, ok := a.chatFor(u, chatID); !ok {
		writeErr(w, http.StatusNotFound, "no such chat")
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	msgs, err := a.DB.WebMessages(r.Context(), chatID, before, 60)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if msgs == nil {
		msgs = []store.WebMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "more": len(msgs) == 60})
}

func (a *Adapter) post(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	sprig, ok := a.chatFor(u, chatID)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such chat")
		return
	}
	text, atts, err := readPost(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if (text == "" && len(atts) == 0) || len(text) > maxTextLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("messages must be 1 to %d characters", maxTextLen))
		return
	}
	a.mu.RLock()
	h, runCtx := a.handler, a.runCtx
	a.mu.RUnlock()
	if h == nil || !a.Connected() {
		writeErr(w, http.StatusServiceUnavailable, "Thicket is still starting up")
		return
	}
	m, err := a.DB.AddWebMessage(r.Context(), u.ID, store.WebMessage{ChatID: chatID, Role: "user", Content: withFileNames(text, atts)})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	a.publish(u.ID, Event{Type: "message", Chat: chatID, Message: &m})
	name := u.Name
	if name == "" {
		name = "you"
	}
	in := channel.Incoming{Channel: ChannelName, ChatID: chatID, MessageID: strconv.FormatInt(m.ID, 10),
		UserID: strconv.FormatInt(u.ID, 10), UserName: name, Text: text, IsDM: true, UserRef: u.ID, Character: sprig, Attachments: atts}
	go h(runCtx, in) // the turn outlives this request
	writeJSON(w, http.StatusOK, map[string]any{"message": m})
}

// deleteMessage deletes a message from a chat and from the conversation
// sprigs see.
func (a *Adapter) deleteMessage(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := a.chatFor(u, chatID); !ok || err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "messages in kids' chats are kept for their parents")
		return
	}
	done, err := a.DB.DeleteWebMessage(r.Context(), chatID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !done {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	a.publish(u.ID, Event{Type: "deleted", Chat: chatID, ID: id})
	if a.onDelete != nil {
		a.onDelete(r.Context(), channel.Deletion{Channel: ChannelName, ChatID: chatID, MessageIDs: []string{strconv.FormatInt(id, 10)}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// pinMessage pins or unpins a message for the rest of its conversation.
func (a *Adapter) pinMessage(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := a.chatFor(u, chatID); !ok || err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if a.Pin == nil {
		writeErr(w, http.StatusServiceUnavailable, "pinning isn't available")
		return
	}
	if _, err := a.DB.WebMessageByID(r.Context(), chatID, id); err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	note, err := a.Pin(r.Context(), u.ID, ChannelName+":"+chatID, strconv.FormatInt(id, 10), body.Pinned)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "couldn't change the pin")
		return
	}
	m, err := a.DB.WebMessageByID(r.Context(), chatID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": m.Pinned, "note": note})
}

// rememberMessage keeps a message as a locked memory ("remember this").
func (a *Adapter) rememberMessage(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := a.chatFor(u, chatID); !ok || err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	if a.Remember == nil {
		writeErr(w, http.StatusServiceUnavailable, "remembering isn't available")
		return
	}
	if _, err := a.DB.WebMessageByID(r.Context(), chatID, id); err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	note, err := a.Remember(r.Context(), u.ID, ChannelName+":"+chatID, strconv.FormatInt(id, 10))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "couldn't remember it")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *Adapter) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// The server's WriteTimeout would cut a long-lived stream.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer
	session, err := r.Cookie(sessionCookie)   // auth already checked it
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	// Close open streams when Thicket shuts down: http.Server.Shutdown
	// doesn't cancel requests, and a stream never goes idle. a.stopped is
	// set up in New, so this holds even for a stream opened before Run.
	stopped := a.stopped
	every := a.PingEvery
	if every <= 0 {
		every = 25 * time.Second
	}
	ch := a.subscribe(u.ID)
	defer a.unsubscribe(u.ID, ch)
	fmt.Fprint(w, "retry: 3000\n: hello\n\n")
	fl.Flush()
	ping := time.NewTicker(every)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stopped:
			return
		case <-ping.C:
			// A session signed out elsewhere, expired or revoked stops
			// receiving; a live one has its expiry slid forward.
			if _, err := a.DB.WebSessionUser(r.Context(), session.Value, SessionTTL); errors.Is(err, store.ErrNotFound) {
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}
