package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// API version 2 is for apps (the iOS app first): the same chats as the web
// app, the calendar, the household and approvals, over bearer tokens. An
// app signs in with a username and password (or a one-time link) and gets
// a token of its own, which the person can revoke.
//
// Every rule is the engine's, not this file's: kids see only their kid
// sprigs, only parents read kid chats, only admins change roles and
// levels, and every change goes in the trail.

// APIVersion is the version /api/v2 reports.
const APIVersion = "2.0"

// Household is what the API asks of the engine.
type Household interface {
	DecideApproval(ctx context.Context, u *store.User, id int64, approve bool) error
	KidChats(ctx context.Context, parent *store.User, sprig string, before int64, limit int) ([]store.KidChatMessage, error)
	KidAccess(ctx context.Context, parent *store.User, sprig string, addTools, removeTools, addAllow, removeAllow []string) (*character.Character, error)
	SetKidLimits(ctx context.Context, parent *store.User, sprig string, l character.KidLimits) (*character.Character, error)
	UndoTrail(ctx context.Context, by *store.User, id int64) (string, error)
	SetToolLevel(ctx context.Context, by *store.User, sprig, tool, level string) error
	SetRole(ctx context.Context, by *store.User, userID int64, role string) error
	AddCalendar(ctx context.Context, u *store.User, name, kind, url, username, password string, shared bool) (*store.CalendarSource, error)
	AddMCPCalendar(ctx context.Context, u *store.User, name string, m calendar.MCPSource, shared bool) (*store.CalendarSource, error)
	RemoveCalendar(ctx context.Context, u *store.User, id int64) error
	RemoveSprigEvent(ctx context.Context, u *store.User, id int64) (*store.SprigEvent, int64, error)
}

// KidSprigMaker creates kid sprigs (the engine's CreateKidSprig, adapted).
type KidSprigMaker func(ctx context.Context, by *store.User, body json.RawMessage) (*character.Character, error)

const approvalMaxAge = 30 * time.Minute

// V2Handler serves /api/v2/.
func (a *Adapter) V2Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2", a.v2Info)
	mux.HandleFunc("GET /api/v2/", a.v2Info)
	mux.HandleFunc("POST /api/v2/auth/login", a.v2Login)
	mux.HandleFunc("POST /api/v2/auth/logout", a.bearer(a.v2Logout))
	mux.HandleFunc("GET /api/v2/me", a.bearer(a.v2Me))
	// Chats: the web app's threads ("<user id>.<sprig>" and "<user id>.all").
	mux.HandleFunc("GET /api/v2/chats/{chat}/messages", a.bearer(a.messages))
	mux.HandleFunc("POST /api/v2/chats/{chat}/messages", a.bearer(a.post))
	mux.HandleFunc("DELETE /api/v2/chats/{chat}/messages/{id}", a.bearer(a.deleteMessage))
	mux.HandleFunc("POST /api/v2/chats/{chat}/messages/{id}/pin", a.bearer(a.pinMessage))
	mux.HandleFunc("GET /api/v2/chats/{chat}/messages/{id}/audio", a.bearer(a.audio))
	mux.HandleFunc("PUT /api/v2/sprigs/{id}/voice", a.bearer(a.setVoice))
	mux.HandleFunc("GET /api/v2/events", a.bearer(a.events))
	// The signed-in person's weekly digest choice.
	mux.HandleFunc("GET /api/v2/digest", a.bearer(a.getDigest))
	mux.HandleFunc("PUT /api/v2/digest", a.bearer(a.setDigest))
	// Calendar.
	mux.HandleFunc("GET /api/v2/calendar", a.bearer(a.v2Calendar))
	mux.HandleFunc("GET /api/v2/calendars", a.bearer(a.v2Calendars))
	mux.HandleFunc("POST /api/v2/calendars", a.bearer(a.v2AddCalendar))
	mux.HandleFunc("DELETE /api/v2/calendars/{id}", a.bearer(a.v2RemoveCalendar))
	mux.HandleFunc("DELETE /api/v2/calendar/outputs/{id}", a.bearer(a.v2RemoveOutput))
	// Approvals and the trail.
	mux.HandleFunc("GET /api/v2/approvals", a.bearer(a.v2Approvals))
	mux.HandleFunc("POST /api/v2/approvals/{id}", a.bearer(a.v2Decide))
	mux.HandleFunc("GET /api/v2/trail", a.bearer(a.v2Trail))
	mux.HandleFunc("POST /api/v2/trail/{id}/undo", a.bearer(a.v2Undo))
	mux.HandleFunc("PUT /api/v2/levels", a.bearer(a.v2SetLevel))
	// Household.
	mux.HandleFunc("GET /api/v2/household", a.bearer(a.v2Household))
	mux.HandleFunc("POST /api/v2/household/invites", a.bearer(a.v2Invite))
	mux.HandleFunc("PATCH /api/v2/household/members/{id}", a.bearer(a.v2SetRole))
	mux.HandleFunc("POST /api/v2/kid-sprigs", a.bearer(a.v2CreateKidSprig))
	mux.HandleFunc("GET /api/v2/kid-sprigs/{id}/chats", a.bearer(a.v2KidChats))
	mux.HandleFunc("PATCH /api/v2/kid-sprigs/{id}/access", a.bearer(a.v2KidAccess))
	mux.HandleFunc("PUT /api/v2/kid-sprigs/{id}/limits", a.bearer(a.v2KidLimits))
	// Memory: facts and relationship memory, yours or a kid sprig's you set up.
	a.memoryRoutes(mux, "/api/v2", a.bearer)
	return mux
}

// bearer resolves an app token. No CSRF header is needed: browsers don't
// attach bearer tokens by themselves.
func (a *Adapter) bearer(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(tok, api.TokenPrefix) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "send Authorization: Bearer <token from POST /api/v2/auth/login>")
			return
		}
		t, err := a.DB.APITokenByHash(r.Context(), api.HashToken(strings.TrimSpace(tok)))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown or revoked token")
			return
		}
		// An agent's token (thicket mcp) only reaches what /api/v1 lets
		// it: here it could answer approvals or run the household.
		if t.Kind == store.TokenMCP {
			writeErr(w, http.StatusForbidden, "this is an agent (mcp) token: it works only with thicket mcp and /api/v1")
			return
		}
		u, err := a.DB.UserByID(r.Context(), t.UserID)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "the token's user no longer exists")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, t)), u)
	}
}

type tokenKey struct{}

func (a *Adapter) v2Info(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; p != "/api/v2" && p != "/api/v2/" {
		writeErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": "thicket", "api": APIVersion,
		"auth": []string{"password", "link"}, "oidc": a.OIDC != nil})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON")
		return false
	}
	return true
}

// v2Login trades a username and password, or a one-time link token, for
// an app token. Lockouts are the web app's.
func (a *Adapter) v2Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Link     string `json:"link"` // a one-time sign-in link, or just its token
		Device   string `json:"device"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ctx, now := r.Context(), time.Now()
	device := strings.TrimSpace(body.Device)
	if device == "" {
		device = "app"
	}
	var uid int64
	verified := "" // the password hash a password sign-in checked against
	switch {
	case body.Link != "":
		token := body.Link
		if i := strings.LastIndex(token, "#"); i >= 0 {
			token = token[i+1:]
		}
		if !a.loginBegin(r, now) {
			writeErr(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
			return
		}
		id, err := a.DB.RedeemWebLink(ctx, token)
		a.loginEnd(r, errors.Is(err, store.ErrNotFound))
		if err != nil || id == 0 { // a first-run owner link sets up in the browser, not an app
			writeErr(w, http.StatusUnauthorized, "That link has expired or was already used. Ask for a new one.")
			return
		}
		uid = id
	default:
		username := strings.TrimSpace(body.Username)
		if a.users.locked(a.clientIP(r), username, now) {
			writeErr(w, http.StatusTooManyRequests, "Too many wrong passwords for that username. Wait 15 minutes and try again.")
			return
		}
		if !a.loginBegin(r, now) {
			writeErr(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
			return
		}
		u, hash, err := a.DB.LoginByUsername(ctx, username)
		verr := auth.VerifyPassword(hash, body.Password)
		ok := err == nil && verr == nil && hash != ""
		a.loginEnd(r, !ok)
		a.users.record(a.clientIP(r), username, ok, now)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Wrong username or password.")
			return
		}
		uid, verified = u.ID, hash
	}
	token, hash, err := api.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "couldn't make a token")
		return
	}
	id, err := a.DB.CreateTokenOfKind(ctx, uid, store.AppTokenPrefix+truncateName(device), store.TokenApp, hash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	// A password change that finished while this sign-in was checked
	// ended the apps' tokens before this one existed. If the password is no
	// longer the one checked, this token goes too. (A change after this
	// check deletes it itself.)
	if verified != "" {
		if _, now, err := a.DB.LoginByUsername(ctx, strings.TrimSpace(body.Username)); err != nil || now != verified {
			_ = a.DB.RevokeAPIToken(ctx, id)
			writeErr(w, http.StatusUnauthorized, "The password changed while signing in. Sign in again.")
			return
		}
	}
	u, _ := a.DB.UserByID(ctx, uid)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "token_id": id, "user": userView(u)})
}

func truncateName(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func (a *Adapter) v2Logout(w http.ResponseWriter, r *http.Request, _ *store.User) {
	t, _ := r.Context().Value(tokenKey{}).(*store.APIToken)
	if t != nil {
		_ = a.DB.RevokeAPIToken(r.Context(), t.ID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func userView(u *store.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{"id": u.ID, "name": u.Name, "role": u.Role, "admin": u.IsAdmin(), "kid": u.IsKid(), "timezone": u.Timezone}
}

func (a *Adapter) v2Me(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user": userView(u), "sprigs": a.sprigViews(r.Context(), u), "parent_of": a.parentOf(r.Context(), u),
		"chats": map[string]string{"room": strconv.FormatInt(u.ID, 10) + "." + roomID, "sprig": strconv.FormatInt(u.ID, 10) + ".<sprig id>"},
	})
}

// parentOf lists the kid sprigs a person set up, with their kids' names.
func (a *Adapter) parentOf(ctx context.Context, u *store.User) []map[string]any {
	out := []map[string]any{}
	if u.IsKid() {
		return out
	}
	for _, c := range a.Chars.All() {
		if !c.IsParent(u.ID) {
			continue
		}
		kids := []map[string]any{}
		for _, id := range c.Kids {
			name := "Kid " + strconv.FormatInt(id, 10)
			if k, err := a.DB.UserByID(ctx, id); err == nil && k.Name != "" {
				name = k.Name
			}
			kids = append(kids, map[string]any{"id": id, "name": name})
		}
		tools, allow := c.Tools, c.Allow
		if tools == nil {
			tools = []string{}
		}
		if allow == nil {
			allow = []string{}
		}
		out = append(out, map[string]any{"id": c.ID, "name": c.DisplayName(), "color": c.Color, "avatar": a.avatarURL(c),
			"kids": kids, "tools": tools, "allow": allow, "limits": c.Limits})
	}
	return out
}

// --- calendar ---

func splitSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}

// v2Calendar: ?from=2026-11-16&to=2026-11-23 (dates or RFC 3339; default
// this week), layers=events,jobs,outputs, sprigs=a,b (only these), hide=c.
func (a *Adapter) v2Calendar(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.Calendar == nil {
		writeErr(w, http.StatusServiceUnavailable, "the calendar isn't available")
		return
	}
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "kids don't have the household calendar")
		return
	}
	loc := u.Location()
	q := r.URL.Query()
	parse := func(s string, def time.Time) (time.Time, error) {
		if s == "" {
			return def, nil
		}
		if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
			return t, nil
		}
		return time.Parse(time.RFC3339, s)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from, err1 := parse(q.Get("from"), today)
	to, err2 := parse(q.Get("to"), from.AddDate(0, 0, 7))
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "from and to are dates (2026-11-16) or RFC 3339 times")
		return
	}
	f := calendar.Filter{Layers: splitSet(q.Get("layers")), Sprigs: splitSet(q.Get("sprigs")), Hide: splitSet(q.Get("hide"))}
	items, errs, err := a.Calendar.View(r.Context(), u, from, to, f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if items == nil {
		items = []calendar.Item{}
	}
	sprigs := map[string]map[string]string{}
	for _, it := range items {
		if it.Sprig == "" || sprigs[it.Sprig] != nil {
			continue
		}
		if c, ok := a.Chars.Get(it.Sprig); ok {
			sprigs[it.Sprig] = map[string]string{"name": c.DisplayName(), "avatar": a.avatarURL(c), "color": c.Color}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "items": items, "sprigs": sprigs, "errors": errs})
}

func (a *Adapter) v2Calendars(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "kids don't have the household calendar")
		return
	}
	srcs, err := a.DB.CalendarSourcesFor(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	out := []map[string]any{}
	for _, s := range srcs {
		c := map[string]any{"id": s.ID, "name": s.Name, "kind": s.Kind, "shared": s.Shared, "mine": s.UserID == u.ID, "last_error": s.LastError}
		if m, err := calendar.ParseMCPSource(s.Config); s.Kind == "mcp" && err == nil {
			c["integration"], c["tool"] = m.Integration, m.Tool
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendars": out})
}

func (a *Adapter) v2AddCalendar(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		Shared   bool   `json:"shared"`
		// kind mcp: the integration, its read-only tool and the mapping.
		MCP calendar.MCPSource `json:"mcp"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	var src *store.CalendarSource
	var err error
	if b.Kind == "mcp" {
		src, err = a.Household.AddMCPCalendar(r.Context(), u, b.Name, b.MCP, b.Shared)
	} else {
		src, err = a.Household.AddCalendar(r.Context(), u, b.Name, b.Kind, b.URL, b.Username, b.Password, b.Shared)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": src.ID, "name": src.Name, "kind": src.Kind, "shared": src.Shared})
}

func (a *Adapter) v2RemoveCalendar(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Household.RemoveCalendar(r.Context(), u, id); err != nil {
		writeErr(w, http.StatusNotFound, "no such calendar of yours")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// v2RemoveOutput takes a sprig output off the calendar. It goes in the
// trail: undo it there (POST /api/v2/trail/{trail_id}/undo) to put it back.
func (a *Adapter) v2RemoveOutput(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "kids can't change the calendar")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ev, trailID, err := a.Household.RemoveSprigEvent(r.Context(), u, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such sprig output you can remove")
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "title": ev.Title, "trail_id": trailID})
	}
}

// --- approvals, trail, levels ---

func (a *Adapter) v2Approvals(w http.ResponseWriter, r *http.Request, u *store.User) {
	aps, err := a.DB.PendingApprovals(r.Context(), u.ID, approvalMaxAge)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	out := []map[string]any{}
	for _, ap := range aps {
		out = append(out, map[string]any{"id": ap.ID, "sprig": ap.Character, "tool": ap.Tool, "args": json.RawMessage(validJSON(ap.Args)), "created_at": ap.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
}

func validJSON(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func (a *Adapter) v2Decide(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b struct {
		Approve *bool `json:"approve"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Approve == nil {
		writeErr(w, http.StatusBadRequest, `send {"approve": true} or {"approve": false}`)
		return
	}
	if err := a.Household.DecideApproval(r.Context(), u, id, *b.Approve); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// v2Trail: admins see everything; others see what concerns them.
func (a *Adapter) v2Trail(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "the trail is for grown-ups")
		return
	}
	who := u.ID
	if u.IsAdmin() {
		who = 0
	}
	entries, err := a.DB.Trail(r.Context(), who, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	out := []store.TrailEntry{}
	for _, e := range entries {
		if a.Chars.PrivateFrom(e.Character, u.ID) {
			continue // someone else's private sprig
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trail": out})
}

func (a *Adapter) v2Undo(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	msg, err := a.Household.UndoTrail(r.Context(), u, id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": msg})
}

func (a *Adapter) v2SetLevel(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		Sprig string `json:"sprig"`
		Tool  string `json:"tool"`
		Level string `json:"level"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if err := a.Household.SetToolLevel(r.Context(), u, b.Sprig, b.Tool, b.Level); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- household ---

func (a *Adapter) v2Household(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "the household list is for grown-ups")
		return
	}
	users, err := a.DB.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	members := []map[string]any{}
	for i := range users {
		members = append(members, userView(&users[i]))
	}
	var sprigs []map[string]any
	for _, c := range a.Chars.All() {
		if c.PrivateFrom(u.ID) {
			// Someone's solo sprig is theirs alone: admins are told only
			// that it exists, and whose it is.
			if u.IsAdmin() {
				sprigs = append(sprigs, map[string]any{"tier": character.TierSolo, "private": true, "belongs_to": c.BelongsTo})
			}
			continue
		}
		v := map[string]any{"id": c.ID, "name": c.DisplayName(), "tier": c.Tier(), "avatar": a.avatarURL(c)}
		if c.IsKid() {
			v["kids"], v["parents"] = c.Kids, c.Parents
		}
		sprigs = append(sprigs, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members, "sprigs": sprigs})
}

func (a *Adapter) v2Invite(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !u.IsAdmin() {
		writeErr(w, http.StatusForbidden, "only the owner or an admin can invite people")
		return
	}
	var b struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		OwnSprig bool   `json:"own_sprig"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Role != store.RoleAdmin && b.Role != store.RoleMember && b.Role != store.RoleKid {
		writeErr(w, http.StatusBadRequest, "role must be admin, member or kid")
		return
	}
	if b.OwnSprig && b.Role == store.RoleKid {
		writeErr(w, http.StatusBadRequest, "own_sprig is for adults: kids get kid sprigs")
		return
	}
	token, err := a.DB.CreateInvite(r.Context(), store.Invite{Role: b.Role, Name: strings.TrimSpace(b.Name), CreatedBy: u.ID, OwnSprig: b.OwnSprig}, 7*24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite_url": strings.TrimRight(a.BaseURL, "/") + "/invite#" + token, "role": b.Role, "own_sprig": b.OwnSprig, "expires_in_days": 7})
}

func (a *Adapter) v2SetRole(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if err := a.Household.SetRole(r.Context(), u, id, b.Role); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Adapter) v2CreateKidSprig(w http.ResponseWriter, r *http.Request, u *store.User) {
	var raw json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	if a.MakeKidSprig == nil {
		writeErr(w, http.StatusServiceUnavailable, "not available")
		return
	}
	c, err := a.MakeKidSprig(r.Context(), u, raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "name": c.DisplayName(), "kids": c.Kids, "parents": c.Parents})
}

func (a *Adapter) v2KidChats(w http.ResponseWriter, r *http.Request, u *store.User) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	msgs, err := a.Household.KidChats(r.Context(), u, r.PathValue("id"), before, 100)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	type kidMessage struct {
		ID      int64     `json:"id"`
		Role    string    `json:"role"` // user (the kid) | assistant (a sprig)
		Sprig   string    `json:"sprig,omitempty"`
		KidID   int64     `json:"kid_id"`
		Content string    `json:"content"`
		At      time.Time `json:"at"`
	}
	out := make([]kidMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, kidMessage{ID: m.ID, Role: m.Role, Sprig: m.Character, KidID: m.KidID, Content: m.Content, At: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "more": len(msgs) == 100})
}

// v2KidLimits replaces a kid sprig's time limits (its parents only).
func (a *Adapter) v2KidLimits(w http.ResponseWriter, r *http.Request, u *store.User) {
	// A pointer, so a JSON null is refused rather than read as "no limits";
	// {} is how limits are cleared.
	var l *character.KidLimits
	if !decodeBody(w, r, &l) {
		return
	}
	if l == nil {
		writeErr(w, http.StatusBadRequest, "send the limits as an object; {} clears them")
		return
	}
	c, err := a.Household.SetKidLimits(r.Context(), u, r.PathValue("id"), *l)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "limits": c.Limits})
}

func (a *Adapter) v2KidAccess(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		AddTools    []string `json:"add_tools"`
		RemoveTools []string `json:"remove_tools"`
		AddAllow    []string `json:"add_allow"`
		RemoveAllow []string `json:"remove_allow"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	c, err := a.Household.KidAccess(r.Context(), u, r.PathValue("id"), b.AddTools, b.RemoveTools, b.AddAllow, b.RemoveAllow)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "tools": c.Tools, "allow": c.Allow})
}
