package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHello(t *testing.T) {
	tok := strings.Repeat("t", 40)
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(tok+"\n"), 0o644)
	srv := httptest.NewServer((&server{tokenFile: file}).handler())
	defer srv.Close()

	call := func(auth, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := call("wrong", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if _, out := call(tok, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); !strings.Contains(out, `"version":"`+version) {
		t.Fatalf("initialize: %s", out)
	}
	if _, out := call(tok, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"say_hello","arguments":{"name":"Wren"}}}`); !strings.Contains(out, "Hello, Wren!") {
		t.Fatalf("call: %s", out)
	}
	if resp, err := http.Get(srv.URL + "/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v", err)
	}
}
