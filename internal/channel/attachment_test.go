package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.png":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("png!"))
		case "/huge":
			w.Write([]byte(strings.Repeat("x", 2000)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := fetchClient.Transport
	fetchClient.Transport = srv.Client().Transport
	defer func() { fetchClient.Transport = old }()
	ctx := context.Background()

	body, ct, err := Fetch(ctx, Attachment{Name: "a.png", URL: srv.URL + "/a.png"}, 100)
	if err != nil || string(body) != "png!" || ct != "image/png" {
		t.Fatalf("fetch: %q %q %v", body, ct, err)
	}
	// The platform's size report is trusted for an early no; the real
	// body is capped regardless of what was reported.
	if _, _, err := Fetch(ctx, Attachment{URL: srv.URL + "/huge", Size: 5000}, 1000); err == nil {
		t.Fatal("reported size over the limit was fetched")
	}
	if _, _, err := Fetch(ctx, Attachment{URL: srv.URL + "/huge", Size: 10}, 1000); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("body over the limit: %v", err)
	}
	if _, _, err := Fetch(ctx, Attachment{URL: "http://example.com/a.png"}, 1000); err == nil {
		t.Fatal("plain http fetched")
	}
	if _, _, err := Fetch(ctx, Attachment{URL: srv.URL + "/missing"}, 1000); err == nil {
		t.Fatal("404 treated as success")
	}
}

func TestFetchCapsOpen(t *testing.T) {
	big := Attachment{Name: "x.png", Open: func(context.Context, int) ([]byte, string, error) { return make([]byte, 50), "image/png", nil }}
	if _, _, err := Fetch(context.Background(), big, 10); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("an adapter returning more than max: %v", err)
	}
}
