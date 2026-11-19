package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func getDashboard(t *testing.T, srv *httptest.Server, session string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/dashboard", nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestDashboard(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.Engine.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	if _, err := a.DB.DB().Exec("INSERT INTO memories (user_id, kind, content, source, created_at, updated_at) VALUES (?, 'fact', ?, 'user', 0, 0)",
		owner.ID, `<script>alert(1)</script> likes tea`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.DB().Exec("INSERT INTO memories (user_id, kind, content, source, due_at, created_at, updated_at) VALUES (?, 'followup', 'Ask how the interview went.', 'auto', ?, 0, 0)",
		owner.ID, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Morning digest", Schedule: "30 7 * * 1-5", Prompt: "x"})
	fc.take()
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()

	// Signed out (or a made-up session): how to sign in, strict headers.
	for _, s := range []string{"", "not-a-session"} {
		resp, body := getDashboard(t, srv, s)
		if resp.StatusCode != 401 || !strings.Contains(body, "web app link") {
			t.Fatalf("signed out: %d %s", resp.StatusCode, body)
		}
		for h, want := range map[string]string{"Content-Security-Policy": "default-src 'none'", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if !strings.Contains(resp.Header.Get(h), want) {
				t.Errorf("header %s = %q", h, resp.Header.Get(h))
			}
		}
	}

	// A web app session opens it.
	session, err := a.DB.CreateWebSession(ctx, owner.ID, "test", web.SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := getDashboard(t, srv, session)
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	for _, want := range []string{"Juni", "Has a dog named Biscuit.", "Morning digest", "30 7 * * 1-5", `id="costs"`, "What chat replies re-send", "history_tokens automatic", `id="trail"`, `id="inbox"`, "Health", `href="/"`, `class="sprig is-`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// Editing a follow-up keeps it one: its own kind comes first, selected.
	if i := strings.Index(body, "Ask how the interview went."); i < 0 ||
		!strings.Contains(body[i:], `<select name="mkind" aria-label="Kind"><option value="followup" selected>followup</option><option value="`) {
		t.Errorf("a follow-up's edit form doesn't keep its kind")
	}
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("memory content isn't escaped")
	}

	// A member sees only their own things.
	member, _ := a.DB.CreateUser(ctx, "discord", "m1", "Kim", "member")
	ms, _ := a.DB.CreateWebSession(ctx, member.ID, "test", web.SessionTTL)
	_, body = getDashboard(t, srv, ms)
	if strings.Contains(body, "Biscuit") || strings.Contains(body, "Morning digest") || strings.Contains(body, `id="costs"`) || strings.Contains(body, `id="health"`) || strings.Contains(body, "called remember") {
		t.Fatal("a member sees the owner's memories, jobs or admin sections")
	}
	if !strings.Contains(body, "Kim") {
		t.Fatal("member's dashboard didn't load")
	}

	// Signing out of the web app ends dashboard access too.
	a.DB.DeleteWebSession(ctx, session)
	if resp, _ := getDashboard(t, srv, session); resp.StatusCode != 401 {
		t.Fatalf("after sign out: %d", resp.StatusCode)
	}
}
