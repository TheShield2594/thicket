package plugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserCheck(t *testing.T) {
	report := `{"checks":[{"name":"chromium","ok":true,"detail":"Chromium 157 started"},{"name":"proxy_blocks_unlisted","ok":true,"detail":"refused"},{"name":"proxy_blocks_private","ok":true,"detail":"metadata refused"}],"open":[],"max":2}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/doctor" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, report)
	}))
	defer srv.Close()
	note, err := Browser.Check(context.Background(), srv.URL, "tok")
	if err != nil || !strings.Contains(note, "0 of 2 browsers open") || !strings.Contains(note, "Chromium 157") {
		t.Fatalf("%q %v", note, err)
	}
	report = `{"checks":[{"name":"chromium","ok":true,"detail":"ok"},{"name":"proxy_blocks_unlisted","ok":false,"detail":"a host not on the allowlist was NOT refused"}],"open":[],"max":2}`
	if _, err := Browser.Check(context.Background(), srv.URL, "tok"); err == nil || !strings.Contains(err.Error(), "NOT refused") {
		t.Fatalf("a failing proxy passed: %v", err)
	}
	if _, err := Browser.Check(context.Background(), srv.URL, "wrong"); err == nil {
		t.Fatal("a refused token passed")
	}
	if Builtin().KidRefused([]string{"mcp:browser/*"}) == nil {
		t.Fatal("kids may use the browser")
	}
}
