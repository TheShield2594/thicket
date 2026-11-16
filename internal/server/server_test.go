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
