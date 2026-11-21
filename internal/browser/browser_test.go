package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowEntry(t *testing.T) {
	for in, want := range map[string]string{
		"shop.example":                      "shop.example",
		"  Shop.Example. ":                  "shop.example",
		"https://www.Shop.example/cart?x=1": "www.shop.example",
		"http://shop.example:8080/":         "shop.example",
		"shop.example/path":                 "shop.example",
		"shop.example:443":                  "shop.example",
		"*.shop.example":                    "shop.example",
		".shop.example":                     "shop.example",
		"xn--bcher-kva.example":             "xn--bcher-kva.example",
	} {
		got, err := AllowEntry(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "nas", "192.168.1.1", "8.8.8.8", "[::1]", "http://10.0.0.1/", "router.lan", "box.local",
		"metadata.google.internal", "x.home.arpa", "a*.example", "*", "has space.example", "bücher.example", "-bad-.example", "http://"} {
		if got, err := AllowEntry(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	list := []string{"shop.example"}
	if !Allowed(list, "shop.example") || !Allowed(list, "WWW.shop.example.") || Allowed(list, "badshop.example") || Allowed(list, "shop.example.evil") {
		t.Fatal("Allowed")
	}
	if ProfileKeySecret("Pip-2") != "browser_pip_2_profile_key" {
		t.Fatal(ProfileKeySecret("Pip-2"))
	}
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/doctor":
			io.WriteString(w, `{"checks":[{"name":"chromium","ok":true,"detail":"ok"}],"open":[{"sprig":"pip"}],"max":2}`)
		case r.Method == "GET" && r.URL.Path == "/v1/profiles":
			io.WriteString(w, `{"profiles":[{"sprig":"pip","size":40,"open":true}]}`)
		case r.Method == "GET" && r.URL.Path == "/v1/profiles/pip":
			io.WriteString(w, "TBP1-sealed")
		case r.Method == "PUT" && r.URL.Path == "/v1/profiles/pip":
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"that sprig's browser is open"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{URL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	d, err := c.Doctor(ctx)
	if err != nil || len(d.Checks) != 1 || !d.Checks[0].OK || len(d.Open) != 1 || d.Max != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	ps, err := c.Profiles(ctx)
	if err != nil || len(ps) != 1 || !ps[0].Open {
		t.Fatalf("%+v %v", ps, err)
	}
	if b, err := c.Profile(ctx, "pip"); err != nil || string(b) != "TBP1-sealed" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := c.Profile(ctx, "juni"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := c.PutProfile(ctx, "pip", []byte("x")); err == nil || err.Error() != "browser plugin: that sprig's browser is open" {
		t.Fatal(err)
	}
	bad := &Client{URL: srv.URL, Token: func(context.Context) (string, error) { return "nope", nil }}
	if _, err := bad.Doctor(ctx); err == nil {
		t.Fatal("a wrong token worked")
	}
}
