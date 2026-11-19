package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/mcp/mcptest"
)

func TestOAuthIntegration(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	srv := mcptest.NewServer(t)

	a.Engine.HandleIncoming(ctx, dm("connect notes at "+srv.URL+"/mcp"))
	out := fc.take()
	var link string
	for _, m := range out {
		if l := regexp.MustCompile(`https?://\S+/auth/authorize\S+`).FindString(m.text); l != "" {
			link = l
		}
	}
	if link == "" {
		t.Fatalf("no sign-in link posted: %+v", out)
	}
	stored, _ := a.DB.MCPServers(ctx)
	if len(stored) != 1 || stored[0].Headers["Authorization"] != mcp.OAuthHeader {
		t.Fatalf("stored server: %+v", stored)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	if !strings.HasSuffix(q.Get("redirect_uri"), "/oauth/callback") {
		t.Fatalf("redirect: %s", q.Get("redirect_uri"))
	}
	srv.SetChallenge(q.Get("code_challenge"))

	// A forged callback gets an error page.
	rec := httptest.NewRecorder()
	a.oauthCallback(rec, httptest.NewRequest("GET", "/oauth/callback?state=nope&code=good-code", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("forged callback: %d %s", rec.Code, rec.Body.String())
	}
	// The real one signs in, connects, and Juni picks it back up.
	rec = httptest.NewRecorder()
	a.oauthCallback(rec, httptest.NewRequest("GET", "/oauth/callback?state="+url.QueryEscape(q.Get("state"))+"&code=good-code", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "signed in to notes") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "Juni's follow-up", func() bool {
		for _, m := range fc.peek() {
			if strings.Contains(m.text, "Who should get the notes tools?") {
				return true
			}
		}
		return false
	})
	if st := a.MCP.Status()["notes"]; st != "connected, 1 tools" {
		t.Fatalf("status after sign-in: %q", st)
	}
	// The token is stored encrypted, never in the server's config.
	if raw, _ := a.Secrets.Get(ctx, mcp.OAuthSecretName("notes")); !strings.Contains(raw, "at-1") {
		t.Fatal("token not stored")
	}
	assertNoLeak(t, a, "at-1")

}
