// Package mcptest has a fake MCP server behind OAuth for tests: protected
// resource and authorization server metadata, dynamic registration, a
// PKCE-checking token endpoint (code "good-code", refresh token "rt") and
// one tool, "search".
package mcptest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Server is a fake MCP server behind OAuth, with its authorization
// server on the same host (under /auth).
type Server struct {
	mu        sync.Mutex
	URL       string
	srv       *httptest.Server
	challenge string
	valid     map[string]bool
	refreshes int
	issued    int
}

// NewServer starts one; it stops when the test ends.
func NewServer(t *testing.T) *Server {
	f := &Server{valid: map[string]bool{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	base := func() string { return f.srv.URL }
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": base() + "/mcp", "authorization_servers": []string{base() + "/auth"}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": base() + "/auth", "authorization_endpoint": base() + "/auth/authorize", "token_endpoint": base() + "/auth/token",
			"registration_endpoint": base() + "/auth/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("POST /auth/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["token_endpoint_auth_method"] != "none" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(201)
		writeJSON(w, map[string]any{"client_id": "cid", "redirect_uris": req["redirect_uris"]})
	})
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		bad := func(code string) { w.WriteHeader(400); writeJSON(w, map[string]string{"error": code}) }
		if r.Form.Get("client_id") != "cid" || r.Form.Get("resource") != base()+"/mcp" {
			bad("invalid_client")
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				bad("invalid_grant")
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt" {
				bad("invalid_grant")
				return
			}
			f.refreshes++
		default:
			bad("unsupported_grant_type")
			return
		}
		f.issued++
		tok := "at-" + string(rune('0'+f.issued))
		f.valid[tok] = true
		writeJSON(w, map[string]any{"access_token": tok, "refresh_token": "rt", "token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		f.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", resource_metadata="`+base()+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(401)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "search", "inputSchema": map[string]any{"type": "object"}}}}
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	})
	return f
}

func (f *Server) Revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
}

// SetChallenge records the PKCE challenge from the sign-in link (what the
// authorize page would have kept).
func (f *Server) SetChallenge(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.challenge = c
}

// Refreshes is how many refresh grants were served.
func (f *Server) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}
