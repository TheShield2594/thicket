package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/mcp/mcptest"
	"github.com/theshield2594/thicket/internal/store"
)

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Get(_ context.Context, n string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[n]
	if !ok {
		return "", errors.New("not set")
	}
	return v, nil
}
func (s *memSecrets) Put(_ context.Context, n, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[n] = v
	return nil
}
func (s *memSecrets) Delete(_ context.Context, n string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, n)
	return nil
}
func (s *memSecrets) Remember(string)                                    {}
func (s *memSecrets) Expand(_ context.Context, v string) (string, error) { return v, nil }

func TestOAuthSignInAndRefresh(t *testing.T) {
	f := mcptest.NewServer(t)
	sec := &memSecrets{m: map[string]string{}}
	now := time.Now()
	o := &OAuth{Secrets: sec, RedirectURL: "https://thicket.example/oauth/callback", Now: func() time.Time { return now }}
	ctx := context.Background()

	done := make(chan error, 1)
	link, err := o.Start(ctx, "notes", f.URL+"/mcp", "", "", func(_ context.Context, err error) { done <- err })
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	if u.Path != "/auth/authorize" || q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != o.RedirectURL || q.Get("resource") != f.URL+"/mcp" || q.Get("state") == "" {
		t.Fatalf("auth link: %s", link)
	}
	f.SetChallenge(q.Get("code_challenge"))

	// Not signed in yet: connecting fails cleanly.
	reg := NewRegistry(nil, sec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg.OAuth = o
	server := store.MCPServer{Name: "notes", Transport: "http", Target: f.URL + "/mcp", Headers: map[string]string{"Authorization": OAuthHeader}, Enabled: true}
	if _, err := reg.Connect(ctx, server); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("connect before sign-in: %v", err)
	}

	// A wrong state is refused; the right one works once.
	if _, err := o.Callback(ctx, "forged", "good-code", "", ""); err == nil {
		t.Fatal("forged state accepted")
	}
	name, err := o.Callback(ctx, q.Get("state"), "good-code", "", "")
	if err != nil || name != "notes" {
		t.Fatalf("callback: %q %v", name, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("done: %v", err)
	}
	if _, err := o.Callback(ctx, q.Get("state"), "good-code", "", ""); err == nil {
		t.Fatal("state reused")
	}
	if raw := sec.m[OAuthSecretName("notes")]; !strings.Contains(raw, `"access_token":"at-1"`) || !strings.Contains(raw, `"refresh_token":"rt"`) {
		t.Fatalf("stored tokens: %s", raw)
	}

	tools, err := reg.Connect(ctx, server)
	if err != nil || len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("connect: %v %v", tools, err)
	}
	// Expired: refreshed before use.
	now = now.Add(2 * time.Hour)
	if tok, err := o.AccessToken(ctx, "notes", false); err != nil || tok != "at-2" || f.Refreshes() != 1 {
		t.Fatalf("refresh on expiry: %q %v (%d refreshes)", tok, err, f.Refreshes())
	}
	// Revoked at the server: a 401 refreshes and the call goes through.
	f.Revoke()
	if _, err := reg.Call(ctx, "notes", "search", nil, true); err != nil {
		t.Fatalf("call after revocation: %v", err)
	}
	if f.Refreshes() != 2 {
		t.Fatalf("expected a refresh after the 401, got %d", f.Refreshes())
	}
	// The refresh token stops working: the person must sign in again.
	sec.m[OAuthSecretName("notes")] = strings.Replace(sec.m[OAuthSecretName("notes")], `"refresh_token":"rt"`, `"refresh_token":"gone"`, 1)
	if _, err := o.AccessToken(ctx, "notes", true); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("dead refresh token: %v", err)
	}
	if err := o.Forget(ctx, "notes"); err != nil || sec.m[OAuthSecretName("notes")] != "" {
		t.Fatal("tokens not forgotten")
	}
}

func TestOAuthSignInErrors(t *testing.T) {
	f := mcptest.NewServer(t)
	o := &OAuth{Secrets: &memSecrets{m: map[string]string{}}, RedirectURL: "https://thicket.example/oauth/callback"}
	ctx := context.Background()
	done := make(chan error, 1)
	link, err := o.Start(ctx, "notes", f.URL+"/mcp", "", "", func(_ context.Context, err error) { done <- err })
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	// The person declined at the sign-in page.
	if _, err := o.Callback(ctx, u.Query().Get("state"), "", "access_denied", "The user said no"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("declined: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("done not told about the failure")
	}
	// A server that needs no sign-in.
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer open.Close()
	if _, err := o.Start(ctx, "open", open.URL, "", "", nil); err == nil || !strings.Contains(err.Error(), "doesn't need OAuth") {
		t.Fatalf("open server: %v", err)
	}
}

func TestWWWAuthParam(t *testing.T) {
	h := []string{`Bearer realm="x", resource_metadata="https://a.example/.well-known/oauth-protected-resource"`}
	if got := wwwAuthParam(h, "resource_metadata"); got != "https://a.example/.well-known/oauth-protected-resource" {
		t.Fatalf("got %q", got)
	}
	if got := wwwAuthParam([]string{`Bearer resource_metadata="u"`}, "resource_metadata"); got != "u" {
		t.Fatalf("first param: %q", got)
	}
}
