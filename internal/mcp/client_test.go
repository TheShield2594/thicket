package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer speaks just enough Streamable HTTP MCP, answering tools/call
// as an SSE stream to exercise both response styles.
func fakeServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var req rpcRequest
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Mcp-Session-Id", "s1")
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, *req.ID)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				t.Error("session id not sent back")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"get_state","description":"read","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},{"name":"turn_on","description":"act","inputSchema":{"type":"object"}}]}}`, *req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"light is on\"}]}}\n\n", *req.ID)
		}
	}))
}

func TestHTTPClient(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()
	ctx := context.Background()
	if _, err := DialHTTP(ctx, srv.URL, nil); err == nil {
		t.Fatal("expected auth failure")
	}
	c, err := DialHTTP(ctx, srv.URL, map[string]string{"Authorization": "Bearer tok"})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	if !tools[0].ReadOnly() || tools[1].ReadOnly() {
		t.Fatal("readOnlyHint not parsed")
	}
	res, err := c.CallTool(ctx, "turn_on", json.RawMessage(`{"entity":"light.kitchen"}`))
	if err != nil || res.Text != "light is on" {
		t.Fatalf("call: %+v %v", res, err)
	}
	if got := FuncName("ha", "turn_on"); got != "mcp_ha__turn_on" {
		t.Fatalf("plain name: %s", got)
	}
	// Names that sanitise to the same thing, or are truncated, must not collide.
	long := strings.Repeat("x", 80)
	names := map[string]bool{}
	for _, pair := range [][2]string{{"ha", "turn_on"}, {"ha", "turn.on"}, {"ha", "turn on"}, {"ha", long + "a"}, {"ha", long + "b"}, {"a_b", "c"}, {"a", "b_c"}} {
		n := FuncName(pair[0], pair[1])
		if names[n] || len(n) > 64 {
			t.Fatalf("collision or too long: %s", n)
		}
		names[n] = true
	}
}
