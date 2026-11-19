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
	o := &OAuth{}
	// A home-network request first, leaving a connection open to reuse...
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := o.client(req.Context()).Do(req)
	if err != nil {
		t.Fatalf("a home-network server's request: %v", err)
	}
	resp.Body.Close()
	// ...which a public-only request to the same place must not use.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := o.client(req.Context()).Do(req); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("public-only request reached localhost: %v", err)
	}
	// An unresolvable host is held to public addresses, not let through.
	if !publicHost(context.Background(), "https://no-such-host.invalid/mcp") {
		t.Fatal("a failed lookup lifted the check")
	}
}
