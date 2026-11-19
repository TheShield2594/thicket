package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMCPServer(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer thk_api_test" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/messages":
			io.WriteString(w, `{"conversation":"mcp","replies":[{"sprig_id":"pip","sprig":"Pip","text":"Dentist at 3."}]}`)
		case "GET /api/v1/sprigs":
			io.WriteString(w, `{"sprigs":[{"id":"pip","name":"Pip","role":"calendar"}]}`)
		case "POST /api/v1/jobs/4/run":
			w.WriteHeader(202)
		case "POST /api/v1/jobs/9/run":
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"that job is paused"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &client{url: srv.URL, token: "thk_api_test", http: srv.Client()}

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_sprig","arguments":{"message":"what's on?","sprig":"pip"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sprigs","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"run_job","arguments":{"id":4}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"run_job","arguments":{"id":9}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"delete_everything"}}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
		`not json`,
	}, "\n")
	pr, pw := io.Pipe()
	done := make(chan error)
	go func() { err := serveMCP(strings.NewReader(in), pw, c); pw.Close(); done <- err }()
	got := map[string]map[string]any{}
	sc := bufio.NewScanner(pr)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q", sc.Text())
		}
		got[string(mustJSON(m["id"]))] = m
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 { // 8 requests with ids, plus the parse error; no answer to the notification
		t.Fatalf("%d responses: %v", len(got), got)
	}
	result := func(id string) map[string]any {
		t.Helper()
		r, ok := got[id]["result"].(map[string]any)
		if !ok {
			t.Fatalf("response %s: %v", id, got[id])
		}
		return r
	}
	toolText := func(id string) (string, bool) {
		r := result(id)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if r := result("1"); r["protocolVersion"] != "2025-03-26" || r["serverInfo"].(map[string]any)["name"] != "thicket" {
		t.Fatalf("initialize: %v", r)
	}
	if n := len(result("2")["tools"].([]any)); n != len(mcpTools) {
		t.Fatalf("tools/list: %d tools", n)
	}
	if text, isErr := toolText("3"); isErr || text != "Pip (pip): Dentist at 3." {
		t.Fatalf("ask_sprig: %q %v", text, isErr)
	}
	if text, _ := toolText("4"); !strings.Contains(text, `"id":"pip"`) {
		t.Fatalf("list_sprigs: %q", text)
	}
	if text, isErr := toolText("5"); isErr || !strings.HasPrefix(text, "Started") {
		t.Fatalf("run_job: %q", text)
	}
	if text, isErr := toolText("6"); !isErr || !strings.Contains(text, "paused") {
		t.Fatalf("paused job: %q %v", text, isErr)
	}
	if e := got["7"]["error"].(map[string]any); e["code"] != float64(-32602) {
		t.Fatalf("unknown tool: %v", got["7"])
	}
	if e := got["8"]["error"].(map[string]any); e["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", got["8"])
	}
	if e := got["null"]["error"].(map[string]any); e["code"] != float64(-32700) {
		t.Fatalf("parse error: %v", got["null"])
	}
	// Every message the server sends can't answer an approval.
	mu.Lock()
	defer mu.Unlock()
	for _, b := range bodies {
		if _, ok := b["text"]; ok && (b["no_approvals"] != true || b["conversation"] != "mcp") {
			t.Fatalf("message sent as %v", b)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
