package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.20": false, "172.16.5.4": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fd00::1": false, "::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v", addr, got)
		}
	}
}

func TestFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret admin page")) }))
	defer srv.Close()
	f := &Fetcher{}
	for _, u := range []string{srv.URL, "http://localhost:" + srv.URL[strings.LastIndex(srv.URL, ":")+1:], "http://169.254.169.254/latest/meta-data/"} {
		if _, err := f.Fetch(context.Background(), u); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "http://user:pw@example.com/", "not a url"} {
		if _, err := f.Fetch(context.Background(), u); err == nil || errors.Is(err, ErrBlocked) {
			t.Errorf("%s should be refused before connecting: %v", u, err)
		}
	}
}

func TestFetchRedirectToPrivateIsBlocked(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("internal")) }))
	defer inner.Close()
	// The public-looking hop is itself local in a test, so allow the first
	// connection only: the dial check must still stop the redirect.
	calls := 0
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer outer.Close()
	f := &Fetcher{}
	_, err := f.Fetch(context.Background(), outer.URL)
	if !errors.Is(err, ErrBlocked) || calls != 0 {
		t.Fatalf("redirect chain: %v (outer reached %d times)", err, calls)
	}
}

func TestFetchPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<!doctype html><html><head><title>Tomato &amp; basil</title><style>p{}</style></head>
<body><nav>Home | About</nav><h1>Growing tomatoes</h1><p>Water <b>deeply</b>&nbsp;once a week.</p>
<script>steal()</script><ul><li>Sun</li><li>Soil</li></ul><table><tr><td>pH</td><td>6.5</td></tr></table><!-- hidden --></body></html>`))
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(strings.Repeat("é", MaxPageChars)))
		case "/bin":
			w.Header().Set("Content-Type", "application/zip")
			w.Write([]byte("PK"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Fetcher{AllowPrivate: true}
	p, err := f.Fetch(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	want := "Growing tomatoes\n\nWater deeply once a week.\n\n- Sun\n- Soil\n\npH 6.5"
	if p.Title != "Tomato & basil" || p.Text != want {
		t.Fatalf("page:\ntitle %q\n%q\nwant\n%q", p.Title, p.Text, want)
	}
	p, err = f.Fetch(context.Background(), srv.URL+"/big")
	if err != nil || !p.Truncated || len(p.Text) > MaxPageChars || !strings.HasSuffix(p.Text, "é") {
		t.Fatalf("big page: %v truncated=%v len=%d", err, p.Truncated, len(p.Text))
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/bin"); err == nil || !strings.Contains(err.Error(), "application/zip") {
		t.Fatalf("binary: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
}

func TestSearXNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example","content":"about ` + r.URL.Query().Get("q") + `"},{"title":"B","url":"https://b.example"},{"title":"C","url":"https://c.example"}]}`))
	}))
	defer srv.Close()
	rs, err := SearXNG(context.Background(), srv.URL, "tomatoes", 2)
	if err != nil || len(rs) != 2 || rs[0].Snippet != "about tomatoes" {
		t.Fatalf("%+v %v", rs, err)
	}
	if got := Format(rs); !strings.Contains(got, "[1] A\nhttps://a.example\nabout tomatoes") {
		t.Fatalf("format: %s", got)
	}
}
