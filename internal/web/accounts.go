package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/store"
)

// Sign-in: one-time links (as before), usernames and passwords, invites
// from the owner, and optionally an OpenID Connect provider. Every way
// ends in the same session cookie, so the web app and /dashboard share it.

// OIDCLogin is a configured OpenID Connect provider.
type OIDCLogin struct {
	Provider *auth.Provider
	// Name labels the button ("Sign in with Authentik").
	Name string
	// GroupsClaim names the claim listing the person's groups. With
	// AdminGroup or MemberGroup set, only people in one of them may sign
	// in, and their role follows the group at each sign-in (the owner's
	// never changes). Either group also lets someone with no account yet
	// in, as a new user.
	GroupsClaim             string
	AdminGroup, MemberGroup string
}

const (
	oidcStateCookie = "thicket_oidc"
	oidcStateTTL    = 10 * time.Minute
	// A username is locked for usernameLockFor after usernameFailLimit
	// wrong passwords in a row.
	usernameFailLimit = 5
	usernameLockFor   = 15 * time.Minute
)

type userThrottle struct {
	mu    sync.Mutex
	fails map[string]int
	until map[string]time.Time
}

func (u *userThrottle) locked(name string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return now.Before(u.until[strings.ToLower(name)])
}

func (u *userThrottle) record(name string, ok bool, now time.Time) {
	key := strings.ToLower(name)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fails == nil {
		u.fails, u.until = map[string]int{}, map[string]time.Time{}
	}
	if ok {
		delete(u.fails, key)
		return
	}
	u.fails[key]++
	if u.fails[key] >= usernameFailLimit {
		u.until[key] = now.Add(usernameLockFor)
		delete(u.fails, key)
	}
	if len(u.fails) > 10000 { // bounded: a flood of made-up names
		u.fails = map[string]int{}
	}
}

// startSession sets the session cookie for a freshly signed-in user.
func (a *Adapter) startSession(w http.ResponseWriter, r *http.Request, uid int64, method string) error {
	ctx := r.Context()
	token, err := a.DB.CreateWebSessionVia(ctx, uid, r.UserAgent(), SessionTTL, method)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(SessionTTL.Seconds()),
		HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	a.Log.Info("web sign-in", "user", uid, "method", method)
	_ = a.DB.LogActivity(ctx, store.Activity{Level: "info", Kind: "system", UserID: uid, Summary: "signed in to the web app (" + method + ")"})
	return nil
}

// authOptions tells the sign-in screen what's on offer.
func (a *Adapter) authOptions(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"password": true}
	if a.OIDC != nil {
		name := a.OIDC.Name
		if name == "" {
			name = "single sign-on"
		}
		out["oidc"] = map[string]string{"name": name}
	}
	writeJSON(w, http.StatusOK, out)
}

// passwordLogin signs in with a username and password.
func (a *Adapter) passwordLogin(w http.ResponseWriter, r *http.Request, username, password string) {
	ctx := r.Context()
	now := time.Now()
	if a.users.locked(username, now) {
		w.Header().Set("Retry-After", "900")
		writeErr(w, http.StatusTooManyRequests, "Too many wrong passwords for that username. Wait 15 minutes and try again.")
		return
	}
	if !a.loginBegin(now) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	u, hash, err := a.DB.LoginByUsername(ctx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.loginEnd(false)
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	// Verified even for a missing user, so timing doesn't tell which
	// usernames exist.
	verr := auth.VerifyPassword(hash, password)
	ok := err == nil && verr == nil && hash != ""
	a.loginEnd(!ok)
	a.users.record(username, ok, now)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	if err := a.startSession(w, r, u.ID, "password"); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// inviteInfo says what an invite is for, so the page can greet them.
func (a *Adapter) inviteInfo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "missing invite")
		return
	}
	inv, err := a.DB.InviteByToken(r.Context(), body.Token)
	if err != nil {
		writeErr(w, http.StatusNotFound, "That invite has expired or was already used. Ask for a new one.")
		return
	}
	out := map[string]any{"name": inv.Name, "role": inv.Role, "existing": inv.UserID != 0}
	if a.OIDC != nil {
		out["oidc"] = map[string]string{"name": a.OIDC.Name}
	}
	writeJSON(w, http.StatusOK, out)
}

// redeemInvite makes the account an invite is for, with a username and
// password, and signs it in.
func (a *Adapter) redeemInvite(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Thicket") != "1" {
		writeErr(w, http.StatusForbidden, "missing X-Thicket header")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if !store.ValidUsername(body.Username) {
		writeErr(w, http.StatusBadRequest, "Usernames are 2 to 32 letters, digits, dots, dashes or underscores.")
		return
	}
	if err := auth.CheckPasswordRules(body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, "Password: "+err.Error()+".")
		return
	}
	if !a.loginBegin(time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		a.loginEnd(false)
		writeErr(w, http.StatusInternalServerError, "couldn't hash the password")
		return
	}
	u, err := a.DB.RedeemInvite(r.Context(), store.HashToken(body.Token), body.Username, hash)
	a.loginEnd(errors.Is(err, store.ErrNotFound))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusUnauthorized, "That invite has expired or was already used. Ask for a new one.")
		return
	case errors.Is(err, store.ErrUsernameTaken):
		writeErr(w, http.StatusConflict, "That username is taken. Try another.")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	_ = a.DB.LogActivity(r.Context(), store.Activity{Level: "info", Kind: "system", UserID: u.ID, Summary: "accepted an invite as " + body.Username})
	if err := a.startSession(w, r, u.ID, "invite"); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// changePassword sets (or first creates) the signed-in user's username
// and password. Other sessions end when a password changes.
func (a *Adapter) changePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Username string `json:"username"`
		Current  string `json:"current"`
		New      string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ctx := r.Context()
	if err := auth.CheckPasswordRules(body.New); err != nil {
		writeErr(w, http.StatusBadRequest, "New password: "+err.Error()+".")
		return
	}
	login, err := a.DB.LoginFor(ctx, u.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	now := time.Now()
	if login != nil && login.HasPassword {
		if a.users.locked(login.Username, now) {
			writeErr(w, http.StatusTooManyRequests, "Too many wrong passwords. Wait 15 minutes and try again.")
			return
		}
		_, hash, _ := a.DB.LoginByUsername(ctx, login.Username)
		if auth.VerifyPassword(hash, body.Current) != nil {
			a.users.record(login.Username, false, now)
			writeErr(w, http.StatusUnauthorized, "Your current password isn't right.")
			return
		}
	}
	hash, err := auth.HashPassword(body.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "couldn't hash the password")
		return
	}
	username := strings.TrimSpace(body.Username)
	if login != nil {
		username = login.Username
	} else if !store.ValidUsername(username) {
		writeErr(w, http.StatusBadRequest, "Choose a username: 2 to 32 letters, digits, dots, dashes or underscores.")
		return
	}
	if err := a.DB.SetLogin(ctx, u.ID, username, hash); errors.Is(err, store.ErrUsernameTaken) {
		writeErr(w, http.StatusConflict, "That username is taken. Try another.")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	ended, _ := a.DB.RevokeUserSessions(ctx, u.ID, keep)
	_ = a.DB.LogActivity(ctx, store.Activity{Level: "info", Kind: "system", UserID: u.ID, Summary: "changed their web app password"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": username, "signed_out": ended})
}

// account is the signed-in user's sign-in details and sessions.
func (a *Adapter) account(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	cur := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		cur = c.Value
	}
	sessions, err := a.DB.WebSessions(ctx, u.ID, cur)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	type sessionView struct {
		ID       string    `json:"id"`
		Device   string    `json:"device"`
		Method   string    `json:"method"`
		Created  time.Time `json:"created"`
		LastSeen time.Time `json:"last_seen"`
		ThisOne  bool      `json:"current"`
	}
	views := []sessionView{}
	for _, s := range sessions {
		views = append(views, sessionView{s.PublicID, Device(s.UserAgent), s.Method, s.CreatedAt, s.LastSeenAt, s.Current})
	}
	out := map[string]any{"sessions": views, "username": "", "has_password": false}
	if l, err := a.DB.LoginFor(ctx, u.ID); err == nil {
		out["username"], out["has_password"] = l.Username, l.HasPassword
	}
	if ok, _ := a.DB.HasOIDC(ctx, u.ID); ok {
		out["oidc"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeSession signs out one of the user's own sessions.
func (a *Adapter) revokeSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	ok, err := a.DB.RevokeWebSession(r.Context(), r.PathValue("id"), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Device is a short description of a browser from its user agent.
func Device(ua string) string {
	browser := "A browser"
	for _, b := range []struct{ key, name string }{{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"curl/", "curl"}} {
		if strings.Contains(ua, b.key) {
			browser = b.name
			break
		}
	}
	for _, o := range []struct{ key, name string }{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Mac OS X", "macOS"}, {"Windows", "Windows"}, {"Linux", "Linux"}} {
		if strings.Contains(ua, o.key) {
			return browser + " on " + o.name
		}
	}
	return browser
}

// ---- OpenID Connect ----

// oidcStart begins a sign-in at the provider and returns where to send
// the browser. It may carry an invite (in the body, never a URL).
func (a *Adapter) oidcStart(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Thicket") != "1" {
		writeErr(w, http.StatusForbidden, "missing X-Thicket header")
		return
	}
	if a.OIDC == nil {
		writeErr(w, http.StatusNotFound, "Single sign-on isn't set up.")
		return
	}
	var body struct {
		Invite string `json:"invite"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	inviteHash := ""
	if body.Invite != "" {
		if _, err := a.DB.InviteByToken(r.Context(), body.Invite); err != nil {
			writeErr(w, http.StatusNotFound, "That invite has expired or was already used. Ask for a new one.")
			return
		}
		inviteHash = store.HashToken(body.Invite)
	}
	state, nonce, verifier := auth.Random(), auth.Random(), auth.Random()
	dest, err := a.OIDC.Provider.AuthURL(r.Context(), state, nonce, verifier)
	if err != nil {
		a.Log.Error("oidc start", "err", err)
		writeErr(w, http.StatusBadGateway, "The sign-in provider isn't answering. Try again, or use your password.")
		return
	}
	if err := a.DB.SaveOIDCState(r.Context(), state, verifier, nonce, inviteHash, oidcStateTTL); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	// The state is tied to this browser too, so a sign-in someone else
	// started can't be finished here (login CSRF).
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: state, Path: "/auth/oidc", MaxAge: int(oidcStateTTL.Seconds()),
		HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]string{"url": dest})
}

func (a *Adapter) oidcFail(w http.ResponseWriter, r *http.Request, msg string) {
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

// oidcCallback finishes a sign-in at the provider.
func (a *Adapter) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if a.OIDC == nil {
		a.oidcFail(w, r, "Single sign-on isn't set up.")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.oidcFail(w, r, "The sign-in provider said no ("+truncate(e, 60)+").")
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(oidcStateCookie)
	if state == "" || err != nil || c.Value != state {
		a.oidcFail(w, r, "That sign-in didn't start in this browser, or took too long. Try again.")
		return
	}
	if !a.loginBegin(time.Now()) {
		a.oidcFail(w, r, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	verifier, nonce, inviteHash, err := a.DB.TakeOIDCState(ctx, state)
	if err != nil {
		a.loginEnd(true)
		a.oidcFail(w, r, "That sign-in expired. Try again.")
		return
	}
	claims, err := a.OIDC.Provider.Exchange(ctx, q.Get("code"), verifier, nonce)
	if err != nil {
		a.loginEnd(true)
		a.Log.Warn("oidc sign-in failed", "err", err)
		a.oidcFail(w, r, "Sign-in failed: "+truncate(err.Error(), 120))
		return
	}
	u, msg, err := a.oidcUser(ctx, claims, inviteHash)
	a.loginEnd(u == nil)
	if err != nil {
		a.Log.Error("oidc user", "err", err)
		a.oidcFail(w, r, "Something went wrong signing you in.")
		return
	}
	if u == nil {
		a.oidcFail(w, r, msg)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteLaxMode})
	if err := a.startSession(w, r, u.ID, "oidc"); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// oidcUser maps verified claims to a Thicket user: an account already
// linked to them, else the invite they came with, else a user with the
// same verified email, else (when groups are configured and they're in
// one) a new user. nil with a message means they may not sign in.
func (a *Adapter) oidcUser(ctx context.Context, cl *auth.Claims, inviteHash string) (*store.User, string, error) {
	o := a.OIDC
	iss := o.Provider.Issuer()
	role := ""
	if o.AdminGroup != "" || o.MemberGroup != "" {
		claim := o.GroupsClaim
		if claim == "" {
			claim = "groups"
		}
		groups := cl.Strings(claim)
		switch {
		case o.AdminGroup != "" && slices.Contains(groups, o.AdminGroup):
			role = "admin"
		case o.MemberGroup != "" && slices.Contains(groups, o.MemberGroup):
			role = "member"
		}
	}
	gated := o.AdminGroup != "" || o.MemberGroup != ""
	email := ""
	if cl.EmailVerified {
		email = cl.Email
	}
	name := cl.Name
	if name == "" {
		name = cl.Username
	}

	u, err := a.DB.OIDCUser(ctx, iss, cl.Subject)
	switch {
	case err == nil:
	case !errors.Is(err, store.ErrNotFound):
		return nil, "", err
	case inviteHash != "":
		if u, err = a.DB.RedeemInvite(ctx, inviteHash, "", ""); errors.Is(err, store.ErrNotFound) {
			return nil, "That invite has expired or was already used. Ask for a new one.", nil
		} else if err != nil {
			return nil, "", err
		}
		if u.Name == "" && name != "" {
			_ = a.DB.UpdateUserProfile(ctx, u.ID, name, "")
		}
	default:
		if email != "" {
			if u, err = a.DB.UserByEmail(ctx, email); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, "", err
			}
		}
		if u == nil && role != "" {
			if u, err = a.DB.CreateWebUser(ctx, name, role); err != nil {
				return nil, "", err
			}
		}
		if u == nil {
			who := cl.Email
			if who == "" {
				who = "this account"
			}
			return nil, "There's no Thicket account for " + who + ". Ask the owner for an invite.", nil
		}
	}
	if gated && u.Role != "owner" {
		if role == "" {
			return nil, "Your account isn't in a group that may use this Thicket.", nil
		}
		if role != u.Role {
			if err := a.DB.SyncUserRole(ctx, u.ID, role); err != nil {
				return nil, "", err
			}
		}
	}
	if err := a.DB.LinkOIDC(ctx, iss, cl.Subject, u.ID, email); err != nil {
		return nil, "", err
	}
	if email != "" && u.Email == "" {
		_ = a.DB.SetUserEmail(ctx, u.ID, email)
	}
	u, err = a.DB.UserByID(ctx, u.ID)
	return u, "", err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
