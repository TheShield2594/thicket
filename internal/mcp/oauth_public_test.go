package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A public MCP server's sign-in URLs can't point Thicket at its own
// network; a server on the home network may use one there.
func TestOAuthClientKeepsPublicServersPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if publicHost(context.Background(), srv.URL) {
		t.Fatal("localhost counted as public")
	}
	ctx := context.WithValue(context.Background(), publicOnlyKey{}, true)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := oauthClient.Do(req); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("public-only request reached localhost: %v", err)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := oauthClient.Do(req)
	if err != nil {
		t.Fatalf("a home-network server's request: %v", err)
	}
	resp.Body.Close()
}
