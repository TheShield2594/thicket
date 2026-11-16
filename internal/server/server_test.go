package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHookRoute(t *testing.T) {
	var gotToken, gotBody, gotCT string
	hooks := func(_ context.Context, token string, body []byte, h http.Header) (int, string) {
		gotToken, gotBody, gotCT = token, string(body), h.Get("Content-Type")
		return http.StatusAccepted, "started"
	}
	srv := httptest.NewServer(New("", "test", t.TempDir(), func(context.Context) map[string]Check { return nil }, Routes{Hooks: hooks, MaxHookBody: 16}).Handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/hooks/thk_abc", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 202 || gotToken != "thk_abc" || gotBody != "hello" || gotCT != "text/plain" || !strings.Contains(string(b), `"message":"started"`) {
		t.Fatalf("got %d %s; token %q body %q ct %q", resp.StatusCode, b, gotToken, gotBody, gotCT)
	}

	resp, _ = http.Post(srv.URL+"/hooks/thk_abc", "text/plain", strings.NewReader(strings.Repeat("x", 17)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/hooks/thk_abc")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

// The web app owns "/" and most of /api/; the local API only /api/v1/.
func TestRoutesDontShadowEachOther(t *testing.T) {
	tag := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(name)) })
	}
	srv := httptest.NewServer(New("", "test", t.TempDir(), func(context.Context) map[string]Check { return nil }, Routes{
		API: tag("api"), Dashboard: tag("dashboard"), App: tag("app"),
	}).Handler)
	defer srv.Close()
	for path, want := range map[string]string{
		"/api/v1/messages": "api", "/api/login": "app", "/api/me": "app", "/api/events": "app",
		"/dashboard": "dashboard", "/": "app", "/c/juniper": "app", "/login": "app",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("%s served by %q, want %q", path, b, want)
		}
	}
}

// An optional check (an integration) failing is reported but doesn't make
// /health fail; a required one does.
func TestHealthOptionalChecks(t *testing.T) {
	checks := map[string]Check{"database": {OK: true}, "mcp.ha": {OK: false, Detail: "error: refused", Optional: true}}
	srv := httptest.NewServer(New("", "test", t.TempDir(), func(context.Context) map[string]Check { return checks }, Routes{}).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"optional":true`) || !strings.Contains(string(b), `"status":"ok"`) {
		t.Fatalf("optional failure: %d %s", resp.StatusCode, b)
	}
	checks["database"] = Check{OK: false}
	resp, _ = http.Get(srv.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("required failure: %d", resp.StatusCode)
	}
}

func TestAvatarRoute(t *testing.T) {
	var gotState string
	av := func(_ context.Context, id, state string) (string, bool) {
		gotState = state
		return `<svg xmlns="http://www.w3.org/2000/svg"/>`, id == "juniper"
	}
	srv := httptest.NewServer(New("", "test", t.TempDir(), func(context.Context) map[string]Check { return nil }, Routes{Avatar: av}).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/avatars/juniper.svg?state=working")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" || gotState != "working" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("avatar: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = http.Get(srv.URL + "/avatars/nobody.svg")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown sprig: %d", resp.StatusCode)
	}
}
