package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/auth/authtest"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func sprigFile(id, name string, belongsTo int64) string {
	owner := ""
	if belongsTo != 0 {
		owner = fmt.Sprintf("belongs_to: %d\n", belongsTo)
	}
	return fmt.Sprintf("---\nid: %s\nname: %s\nrole: Helps with %s things.\n%spersonality:\n  traits: [kind]\nvoice:\n  style: [warm]\n---\n%s is here.\n", id, name, id, owner, name)
}

type accountsEnv struct {
	a       *App
	ctx     context.Context
	srv     *httptest.Server
	owner   *store.User
	member  *store.User
	fc      *fakeChannel
	cleanup func()
}

// accountsSetup: an owner (from Discord), a member with a password, and
// sprigs: Pip (shared), Fern (the member's own), Moss (the owner's own).
func accountsSetup(t *testing.T) *accountsEnv {
	t.Helper()
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	member, err := a.DB.CreateWebUser(ctx, "Kim", "member")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("kims long password")
	if err := a.DB.SetLogin(ctx, member.ID, "kim", hash); err != nil {
		t.Fatal(err)
	}
	for id, f := range map[string]string{"pip": sprigFile("pip", "Pip", 0), "fern": sprigFile("fern", "Fern", member.ID), "moss": sprigFile("moss", "Moss", owner.ID)} {
		if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, id+".md"), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Chars.Load(); err != nil || len(a.Chars.Errors()) > 0 {
		t.Fatalf("characters: %v %v", err, a.Chars.Errors())
	}
	runCtx, stop := context.WithCancel(ctx)
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.Handler())
	t.Cleanup(func() { srv.Close(); stop() })
	return &accountsEnv{a: a, ctx: ctx, srv: srv, owner: owner, member: member, fc: fc}
}

func (e *accountsEnv) client(t *testing.T) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{t: t, base: e.srv.URL, c: &http.Client{Jar: jar}}
}

func sprigIDs(me map[string]any) []string {
	var out []string
	for _, s := range me["sprigs"].([]any) {
		out = append(out, s.(map[string]any)["id"].(string))
	}
	return out
}

func TestPasswordSignInSeesOnlySharedAndOwnSprigs(t *testing.T) {
	e := accountsSetup(t)
	w := e.client(t)
	if code, out := w.do("POST", "/api/login", map[string]string{"username": "kim", "password": "wrong password!"}, true); code != 401 || !strings.Contains(fmt.Sprint(out), "Wrong username or password") {
		t.Fatalf("wrong password: %d %v", code, out)
	}
	if code, _ := w.do("POST", "/api/login", map[string]string{"username": "nobody", "password": "kims long password"}, true); code != 401 {
		t.Fatalf("unknown user: %d", code)
	}
	if code, out := w.do("POST", "/api/login", map[string]string{"username": "KIM", "password": "kims long password"}, true); code != 200 {
		t.Fatalf("sign in: %d %v", code, out)
	}
	_, me := w.do("GET", "/api/me", nil, false)
	if got := strings.Join(sprigIDs(me), ","); got != "juniper,fern,pip" {
		t.Fatalf("member sees %s, want juniper,fern,pip", got)
	}
	moss := fmt.Sprintf("%d.moss", e.member.ID)
	if code, _ := w.do("GET", "/api/chats/"+moss+"/messages", nil, false); code != 404 {
		t.Fatalf("someone else's sprig's thread: %d", code)
	}
	if code, _ := w.do("GET", "/api/sprigs/moss", nil, false); code != 404 {
		t.Fatalf("someone else's sprig's details: %d", code)
	}
	// Naming someone else's sprig in the shared room reaches the host.
	room := fmt.Sprintf("%d.all", e.member.ID)
	w.do("POST", "/api/chats/"+room+"/messages", map[string]string{"text": "Moss, are you there?"}, true)
	var msgs []store.WebMessage
	waitFor(t, "a reply", func() bool { msgs, _ = e.a.DB.WebMessages(e.ctx, room, 0, 10); return len(msgs) == 2 })
	if msgs[1].Character != "juniper" {
		t.Fatalf("someone else's sprig answered: %+v", msgs[1])
	}
	// Their own private sprig answers in its thread.
	fern := fmt.Sprintf("%d.fern", e.member.ID)
	if code, _ := w.do("POST", "/api/chats/"+fern+"/messages", map[string]string{"text": "hi Fern"}, true); code != 200 {
		t.Fatalf("own sprig: %d", code)
	}
	// The owner sees their own private sprig, not the member's.
	ow := e.client(t)
	link, _ := e.a.DB.CreateWebLink(e.ctx, e.owner.ID, time.Hour)
	ow.do("POST", "/api/login", map[string]string{"token": link}, true)
	_, me = ow.do("GET", "/api/me", nil, false)
	if got := strings.Join(sprigIDs(me), ","); got != "juniper,moss,pip" {
		t.Fatalf("owner sees %s", got)
	}
	// In Discord too: the member's sprig won't answer the owner by name.
	e.a.Engine.HandleIncoming(e.ctx, dm("Fern, hello"))
	if out := e.fc.take(); len(out) == 0 || out[len(out)-1].persona == "Fern" {
		t.Fatalf("someone else's sprig answered in Discord: %+v", out)
	}
}

func TestWrongPasswordsLockTheUsername(t *testing.T) {
	e := accountsSetup(t)
	w := e.client(t)
	for i := 0; i < 5; i++ {
		w.do("POST", "/api/login", map[string]string{"username": "kim", "password": fmt.Sprint("guess number ", i)}, true)
	}
	if code, _ := w.do("POST", "/api/login", map[string]string{"username": "kim", "password": "kims long password"}, true); code != http.StatusTooManyRequests {
		t.Fatalf("after 5 wrong passwords: %d", code)
	}
}

func TestInviteAccountAndSessions(t *testing.T) {
	e := accountsSetup(t)
	token, err := e.a.DB.CreateInvite(e.ctx, store.Invite{Role: "member", Name: "Robin", CreatedBy: e.owner.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b1 := e.client(t)
	if code, out := b1.do("POST", "/api/invite/check", map[string]string{"token": token}, true); code != 200 || out["name"] != "Robin" {
		t.Fatalf("invite check: %d %v", code, out)
	}
	if code, _ := b1.do("POST", "/api/invite", map[string]string{"token": token, "username": "robin", "password": "short"}, true); code != 400 {
		t.Fatalf("weak password: %d", code)
	}
	if code, _ := b1.do("POST", "/api/invite", map[string]string{"token": token, "username": "kim", "password": "robins password ok"}, true); code != http.StatusConflict {
		t.Fatalf("taken username: %d", code)
	}
	if code, out := b1.do("POST", "/api/invite", map[string]string{"token": token, "username": "robin", "password": "robins password ok"}, true); code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	_, me := b1.do("GET", "/api/me", nil, false)
	if u := me["user"].(map[string]any); u["role"] != "member" || u["name"] != "Robin" {
		t.Fatalf("invited user: %v", u)
	}
	if code, _ := e.client(t).do("POST", "/api/invite", map[string]string{"token": token, "username": "robin2", "password": "robins password ok"}, true); code != 401 {
		t.Fatalf("invite used twice: %d", code)
	}
	// Juni greets Robin in their chat as soon as they've joined.
	e.a.Engine.WaitTasks()
	robinID := int64(me["user"].(map[string]any)["id"].(float64))
	_, msgs := b1.do("GET", fmt.Sprintf("/api/chats/%d.juniper/messages", robinID), nil, false)
	if got := fmt.Sprint(msgs["messages"]); !strings.Contains(got, "Lovely to meet you") {
		t.Fatalf("no welcome in Robin's chat: %v", msgs)
	}

	// A second browser; both are listed.
	b2 := e.client(t)
	if code, _ := b2.do("POST", "/api/login", map[string]string{"username": "robin", "password": "robins password ok"}, true); code != 200 {
		t.Fatal("password sign-in after invite")
	}
	_, acct := b1.do("GET", "/api/account", nil, false)
	if sessions := acct["sessions"].([]any); len(sessions) != 2 || acct["username"] != "robin" || acct["has_password"] != true {
		t.Fatalf("account: %v", acct)
	}
	// Changing the password signs the other browser out.
	if code, _ := b1.do("POST", "/api/account/password", map[string]string{"current": "wrong current pw", "new": "a brand new password"}, true); code != 401 {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, out := b1.do("POST", "/api/account/password", map[string]string{"current": "robins password ok", "new": "a brand new password"}, true); code != 200 || out["signed_out"] != float64(1) {
		t.Fatalf("change password: %d %v", code, out)
	}
	if code, _ := b2.do("GET", "/api/me", nil, false); code != 401 {
		t.Fatalf("other browser still signed in: %d", code)
	}
	// Revoke a session from the list.
	b3 := e.client(t)
	b3.do("POST", "/api/login", map[string]string{"username": "robin", "password": "a brand new password"}, true)
	_, acct = b1.do("GET", "/api/account", nil, false)
	var other string
	for _, s := range acct["sessions"].([]any) {
		if s.(map[string]any)["current"] != true {
			other = s.(map[string]any)["id"].(string)
		}
	}
	if code, _ := b1.do("DELETE", "/api/account/sessions/"+other, nil, true); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := b3.do("GET", "/api/me", nil, false); code != 401 {
		t.Fatalf("revoked browser still signed in: %d", code)
	}
	// Someone else's session can't be revoked by id guessing.
	kim := e.client(t)
	kim.do("POST", "/api/login", map[string]string{"username": "kim", "password": "kims long password"}, true)
	list, _ := e.a.DB.WebSessions(e.ctx, e.member.ID, "")
	if code, _ := b1.do("DELETE", "/api/account/sessions/"+list[0].PublicID, nil, true); code != 404 {
		t.Fatalf("revoked someone else's session: %d", code)
	}
}

// oidcSignIn runs a sign-in through the fake provider and returns where
// Thicket sent the browser at the end.
func oidcSignIn(t *testing.T, w *webClient, invite string) string {
	t.Helper()
	code, out := w.do("POST", "/api/oidc/start", map[string]string{"invite": invite}, true)
	if code != 200 {
		t.Fatalf("oidc start: %d %v", code, out)
	}
	c := &http.Client{Jar: w.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	next := out["url"].(string)
	for i := 0; i < 3; i++ {
		resp, err := c.Get(next)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		loc, err := resp.Location()
		if err != nil {
			t.Fatalf("no redirect from %s (%d)", next, resp.StatusCode)
		}
		if !strings.Contains(loc.Path, "/auth/oidc/callback") && !strings.HasPrefix(loc.String(), loc.Scheme+"://"+loc.Host+"/authorize") {
			return loc.RequestURI()
		}
		next = loc.String()
	}
	t.Fatal("too many redirects")
	return ""
}

func TestOIDCSignIn(t *testing.T) {
	e := accountsSetup(t)
	fake := authtest.New("thicket", "client-secret")
	defer fake.Close()
	e.a.Web.OIDC = &web.OIDCLogin{
		Provider:    auth.NewProvider(auth.OIDCConfig{Issuer: fake.URL, ClientID: "thicket", ClientSecret: "client-secret", RedirectURL: e.srv.URL + "/auth/oidc/callback"}),
		Name:        "Authentik",
		GroupsClaim: "groups", AdminGroup: "thicket-admins", MemberGroup: "thicket",
	}
	if _, out := e.client(t).do("GET", "/api/auth", nil, false); fmt.Sprint(out["oidc"]) != "map[name:Authentik]" {
		t.Fatalf("auth options: %v", out)
	}

	// Not in an allowed group: refused.
	fake.SetClaims(map[string]any{"sub": "stranger", "email": "x@example.com", "email_verified": true, "groups": []any{"other"}})
	w := e.client(t)
	if dest := oidcSignIn(t, w, ""); !strings.HasPrefix(dest, "/login?error=") {
		t.Fatalf("stranger let in: %s", dest)
	}
	if code, _ := w.do("GET", "/api/me", nil, false); code != 401 {
		t.Fatal("stranger has a session")
	}

	// Kim has an email on file and is in the members group: signs in as Kim.
	e.a.DB.SetUserEmail(e.ctx, e.member.ID, "kim@example.com")
	fake.SetClaims(map[string]any{"sub": "kim-sub", "email": "KIM@example.com", "email_verified": true, "groups": []any{"thicket"}})
	w = e.client(t)
	if dest := oidcSignIn(t, w, ""); dest != "/" {
		t.Fatalf("kim: %s", dest)
	}
	_, me := w.do("GET", "/api/me", nil, false)
	if me["user"].(map[string]any)["id"] != float64(e.member.ID) || strings.Join(sprigIDs(me), ",") != "juniper,fern,pip" {
		t.Fatalf("kim via OIDC: %v", me)
	}
	// Next time by subject, even with another email; the admin group
	// makes her an admin.
	fake.SetClaims(map[string]any{"sub": "kim-sub", "email": "kim@new.example", "email_verified": true, "groups": []any{"thicket-admins"}})
	oidcSignIn(t, e.client(t), "")
	if u, _ := e.a.DB.UserByID(e.ctx, e.member.ID); u.Role != "admin" {
		t.Fatalf("role not synced: %s", u.Role)
	}

	// Someone new in the members group gets an account.
	fake.SetClaims(map[string]any{"sub": "new-sub", "email": "sam@example.com", "email_verified": true, "name": "Sam", "groups": []any{"thicket"}})
	w = e.client(t)
	if dest := oidcSignIn(t, w, ""); dest != "/" {
		t.Fatalf("new member: %s", dest)
	}
	_, me = w.do("GET", "/api/me", nil, false)
	if u := me["user"].(map[string]any); u["role"] != "member" || u["name"] != "Sam" {
		t.Fatalf("new member: %v", u)
	}

	// An unverified email never matches an account.
	e.a.Web.OIDC.AdminGroup, e.a.Web.OIDC.MemberGroup = "", ""
	fake.SetClaims(map[string]any{"sub": "spoof", "email": "kim@example.com", "email_verified": false})
	if dest := oidcSignIn(t, e.client(t), ""); !strings.Contains(dest, "error=") {
		t.Fatalf("unverified email matched: %s", dest)
	}

	// With an invite, a new person gets the invited account.
	token, _ := e.a.DB.CreateInvite(e.ctx, store.Invite{Role: "member", Name: "Ash"}, time.Hour)
	fake.SetClaims(map[string]any{"sub": "ash-sub", "email": "ash@example.com", "email_verified": true})
	w = e.client(t)
	if dest := oidcSignIn(t, w, token); dest != "/" {
		t.Fatalf("invite via OIDC: %s", dest)
	}
	_, me = w.do("GET", "/api/me", nil, false)
	if me["user"].(map[string]any)["name"] != "Ash" {
		t.Fatalf("invited via OIDC: %v", me)
	}

	// A sign-in started in one browser can't be finished in another
	// (login CSRF): the second browser follows the provider's redirect
	// with a real code, but lacks the state cookie.
	fake.SetClaims(map[string]any{"sub": "ash-sub"})
	_, out := e.client(t).do("POST", "/api/oidc/start", map[string]string{}, true)
	noFollow := func(jar http.CookieJar) *http.Client {
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	victim, _ := cookiejar.New(nil)
	resp, err := noFollow(victim).Get(out["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := resp.Location()
	resp, err = noFollow(victim).Get(cb.String())
	if err != nil {
		t.Fatal(err)
	}
	if loc, _ := resp.Location(); loc == nil || !strings.Contains(loc.String(), "error=") {
		t.Fatalf("callback in another browser: %v", loc)
	}
	srvURL, _ := url.Parse(e.srv.URL)
	for _, c := range victim.Cookies(srvURL) {
		if c.Name == "thicket_session" {
			t.Fatal("the other browser got a session")
		}
	}
}

func TestDashboardSessionsAndInvites(t *testing.T) {
	e := accountsSetup(t)
	srv := httptest.NewServer(e.a.Dashboard.Handler())
	defer srv.Close()
	session, _ := e.a.DB.CreateWebSessionVia(e.ctx, e.owner.ID, "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) Firefox/130.0", web.SessionTTL, "password")
	other, _ := e.a.DB.CreateWebSessionVia(e.ctx, e.member.ID, "Mozilla/5.0 (iPhone) Safari/605", web.SessionTTL, "invite")
	_, body := getDashboard(t, srv, session)
	for _, want := range []string{`id="signin"`, "Firefox on macOS", "Safari on iPhone", "Make invite link", "Moss", "Kim&#39;s private sprig"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// Kim's solo sprig is private: the owner sees that it exists, not what it is.
	if strings.Contains(body, "Fern") {
		t.Error("the owner's dashboard names Kim's private sprig")
	}
	csrf := between(body, `name="csrf" value="`, `"`)
	post := func(form url.Values) (int, string) {
		form.Set("csrf", csrf)
		req, _ := http.NewRequest("POST", srv.URL+"/dashboard/act", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "thicket_session", Value: session})
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, page := post(url.Values{"kind": {"invite"}, "action": {"create"}, "role": {"member"}, "name": {"Lee"}, "days": {"3"}})
	link := between(page, "/invite#", "<")
	if code != 200 || link == "" {
		t.Fatalf("invite: %d", code)
	}
	if inv, err := e.a.DB.InviteByToken(e.ctx, link); err != nil || inv.Name != "Lee" {
		t.Fatalf("invite stored: %+v %v", inv, err)
	}
	// The owner signs the member's phone out.
	list, _ := e.a.DB.WebSessions(e.ctx, e.member.ID, "")
	if code, _ := post(url.Values{"kind": {"session"}, "action": {"revoke"}, "sid": {list[0].PublicID}}); code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", code)
	}
	if _, err := e.a.DB.WebSessionUser(e.ctx, other, web.SessionTTL); err == nil {
		t.Fatal("revoked session still works")
	}
	// A member's dashboard: their sprigs, no one else's, no invite form.
	ms, _ := e.a.DB.CreateWebSession(e.ctx, e.member.ID, "x", web.SessionTTL)
	_, body = getDashboard(t, srv, ms)
	if strings.Contains(body, "Moss") || !strings.Contains(body, "Fern") || strings.Contains(body, "Make invite link") || strings.Contains(body, "Everyone&#39;s sessions") {
		t.Fatal("member's dashboard shows someone else's sprig or admin controls")
	}
}

func between(s, a, b string) string {
	_, rest, ok := strings.Cut(s, a)
	if !ok {
		return ""
	}
	out, _, _ := strings.Cut(rest, b)
	return out
}
