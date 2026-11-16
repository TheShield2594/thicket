package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
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
	innerCalls := 0
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { innerCalls++; w.Write([]byte("internal")) }))
	defer inner.Close()
	outerCalls := 0
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outerCalls++
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer outer.Close()
	// The first hop stands in for a public site: only its exact address is
	// let through, so the redirect still meets the production check.
	allowed := strings.TrimPrefix(outer.URL, "http://")
	f := &Fetcher{allowAddr: func(a string) bool { return a == allowed }}
	_, err := f.Fetch(context.Background(), outer.URL)
	if !errors.Is(err, ErrBlocked) || outerCalls != 1 || innerCalls != 0 {
		t.Fatalf("redirect to a private address: err=%v, first hop reached %d times, private target %d times", err, outerCalls, innerCalls)
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

// fakeProxy is a forward proxy for plain-http targets: it answers every
// request itself, recording the absolute URLs it was asked for.
func fakeProxy(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		switch r.URL.Host {
		case "public.example":
			if r.URL.Path == "/hop" {
				http.Redirect(w, r, "http://internal.example/secret", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello from the internet"))
		default:
			w.Write([]byte("internal"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestFetchThroughProxy(t *testing.T) {
	proxy, seen := fakeProxy(t)
	pu, _ := url.Parse(proxy.URL)
	f := &Fetcher{
		Proxy: http.ProxyURL(pu),
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.example":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "mixed.example": // one public, one private: refused
				return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.7")}, nil
			default:
				return []netip.Addr{netip.MustParseAddr("192.168.1.10")}, nil
			}
		},
	}
	p, err := f.Fetch(context.Background(), "http://public.example/page")
	if err != nil || p.Text != "hello from the internet" {
		t.Fatalf("public page through the proxy: %v %+v", err, p)
	}
	for _, target := range []string{"http://internal.example/", "http://mixed.example/", "http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/"} {
		if _, err := f.Fetch(context.Background(), target); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: want ErrBlocked, got %v", target, err)
		}
	}
	// Redirects are checked too.
	if _, err := f.Fetch(context.Background(), "http://public.example/hop"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to a private name: %v", err)
	}
	// The proxy was only ever asked for the public pages.
	for _, u := range seen() {
		if !strings.Contains(u, "public.example") {
			t.Fatalf("proxy was asked for %s", u)
		}
	}
	// A request the proxy setting sends direct still gets the
	// connect-time check, and the proxy's own address isn't reachable
	// that way either.
	direct := &Fetcher{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, AllowPrivate: false}
	if _, err := direct.Fetch(context.Background(), proxy.URL+"/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("direct to a private address: %v", err)
	}
	f.Proxy = func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == pu.Host {
			return nil, nil // as NO_PROXY=127.0.0.1 would
		}
		return pu, nil
	}
	f.allowAddr = func(a string) bool { return a == pu.Host } // pretend the proxy is public
	if _, err := f.Fetch(context.Background(), proxy.URL+"/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("direct to the proxy's own address: %v", err)
	}
}
