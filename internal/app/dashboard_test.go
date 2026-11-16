package app

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/store"
)

var linkRe = regexp.MustCompile(`http://localhost:\d+(/dashboard/login\?t=[A-Za-z0-9_-]+)`)

type browser struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t, srv, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.srv.URL+path, nil)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestDashboard(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.Engine.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	if _, err := a.DB.DB().Exec("INSERT INTO memories (user_id, kind, content, source, created_at, updated_at) VALUES (?, 'fact', ?, 'user', 0, 0)",
		owner.ID, `<script>alert(1)</script> likes tea`); err != nil {
		t.Fatal(err)
	}
	a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Morning digest", Schedule: "30 7 * * 1-5", Prompt: "x"})
	fc.take()
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	b := newBrowser(t, srv)

	// Signed out: a page saying how to sign in, with strict headers.
	resp, body := b.do("GET", "/dashboard")
	if resp.StatusCode != 401 || !strings.Contains(body, "ask Juni for a sign-in link") {
		t.Fatalf("signed out: %d %s", resp.StatusCode, body)
	}
	for h, want := range map[string]string{"Content-Security-Policy": "default-src 'none'", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if !strings.Contains(resp.Header.Get(h), want) {
			t.Errorf("header %s = %q", h, resp.Header.Get(h))
		}
	}

	// Juni sends a one-time link; the model never sees it.
	a.Engine.HandleIncoming(ctx, dm("can I see my dashboard?"))
	var path string
	for _, s := range fc.take() {
		if m := linkRe.FindStringSubmatch(s.text); m != nil {
			path = m[1]
		}
	}
	if path == "" {
		t.Fatal("no link DMed")
	}
	token := path[strings.Index(path, "t=")+2:]
	fm.mu.Lock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, token) {
				t.Error("the sign-in token reached the model")
			}
		}
	}
	fm.mu.Unlock()
	assertNoLeak(t, a, token)

	resp, _ = b.do("GET", path)
	ck := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/dashboard" ||
		!strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") || !strings.Contains(ck, "Path=/dashboard") {
		t.Fatalf("sign in: %d %q %q", resp.StatusCode, resp.Header.Get("Location"), ck)
	}
	if r2, body := newBrowser(t, srv).do("GET", path); r2.StatusCode != 401 || !strings.Contains(body, "already used") {
		t.Fatalf("a link works once: %d", r2.StatusCode)
	}

	resp, body = b.do("GET", "/dashboard")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	for _, want := range []string{"Juni", "Has a dog named Biscuit.", "Morning digest", "30 7 * * 1-5", "Spending", "Recent activity", "Health"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("memory content isn't escaped")
	}

	// A member sees only their own things.
	member, _ := a.DB.CreateUser(ctx, "discord", "m1", "Kim", "member")
	link, _ := dashboard.LoginLink(ctx, a.DB, member.ID, "", a.Cfg.HTTPAddr)
	mb := newBrowser(t, srv)
	mb.do("GET", linkRe.FindStringSubmatch(link)[1])
	_, body = mb.do("GET", "/dashboard")
	if strings.Contains(body, "Biscuit") || strings.Contains(body, "Morning digest") || strings.Contains(body, "Spending") || strings.Contains(body, "Recent activity") {
		t.Fatal("a member sees the owner's memories, jobs or admin sections")
	}
	if !strings.Contains(body, "Kim") {
		t.Fatal("member's dashboard didn't load")
	}

	// Signing out ends the session.
	b.do("POST", "/dashboard/logout")
	if resp, _ := b.do("GET", "/dashboard"); resp.StatusCode != 401 {
		t.Fatalf("after sign out: %d", resp.StatusCode)
	}
}
