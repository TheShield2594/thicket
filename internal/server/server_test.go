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
	hooks := func(_ context.Context, token string, body []byte, ct string) (int, string) {
		gotToken, gotBody, gotCT = token, string(body), ct
		return http.StatusAccepted, "started"
	}
	srv := httptest.NewServer(New("", "test", t.TempDir(), func(context.Context) map[string]Check { return nil }, hooks, 16).Handler)
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
