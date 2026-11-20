// Command hello is the smallest Thicket plugin: a template to copy. It is
// an MCP server (Streamable HTTP, JSON answers) with one tool, behind the
// token Thicket shares through a file both containers mount. See
// docs/plugins.md.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// version is reported to Thicket (and `thicket doctor`) when it connects.
var version = "0.1.0"

func main() {
	addr := env("HELLO_ADDR", ":7071")
	s := &server{tokenFile: env("HELLO_TOKEN_FILE", "/plugin-auth/token")}
	log.Printf("hello plugin %s listening on %s", version, addr)
	log.Fatal(http.ListenAndServe(addr, s.handler()))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	tokenFile string
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	// /health answers without a token: Thicket uses it to see whether the
	// plugin is running at all.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /mcp", s.mcp)
	return mux
}

// authorized checks the bearer token against the shared file, read on
// every request so a new token takes effect without a restart. With no
// token file nothing is allowed.
func (s *server) authorized(r *http.Request) bool {
	b, err := os.ReadFile(s.tokenFile)
	want := strings.TrimSpace(string(b))
	if err != nil || len(want) < 32 {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *server) mcp(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, err := s.answer(req)
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if err != nil {
		resp["error"] = map[string]any{"code": -32601, "message": err.Error()}
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) answer(req request) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "thicket-hello", "version": version},
		}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "say_hello",
			"description": "Greets someone by name.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}},
			// Read-only tools run without asking; everything else asks
			// for the person's OK by default.
			"annotations": map[string]any{"readOnlyHint": true},
		}}}, nil
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Name string `json:"name"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "say_hello" {
			return nil, errors.New("unknown tool")
		}
		name := strings.TrimSpace(p.Arguments.Name)
		if name == "" {
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "name is required"}}}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Hello, " + name + "!"}}}, nil
	case "ping":
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("method %s not found", req.Method)
}
