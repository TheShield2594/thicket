package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
	// from, if set, is the client address a reverse proxy on this
	// machine reports (X-Forwarded-For).
	from string
}

func (w *webClient) do(method, path string, body any, csrf bool) (int, map[string]any) {
	w.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, w.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-Thicket", "1")
	}
	if w.from != "" {
		req.Header.Set("X-Forwarded-For", w.from)
	}
	res, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebApp(t *testing.T) {
	a, _, _, ctx := setup(t)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	if err := a.Secrets.Put(ctx, secrets.OpenRouterKey, "sk-or-test"); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)

	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	w := &webClient{t: t, base: srv.URL, c: &http.Client{Jar: jar}}

	// Signed out.
	if code, _ := w.do("GET", "/api/me", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("me before sign-in: %d", code)
	}
	// The app shell is served for app routes.
	if res, err := http.Get(srv.URL + "/c/pip"); err != nil || res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("shell: %v %v", res, err)
	}

	// A fresh install: the first link makes a web owner. It works once.
	token, err := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := w.do("POST", "/api/login", map[string]string{"token": token}, false); code != http.StatusForbidden {
		t.Fatalf("login without X-Thicket: %d", code)
	}
	if code, out := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, _ := (&webClient{t: t, base: srv.URL, c: &http.Client{}}).do("POST", "/api/login", map[string]string{"token": token}, true); code != http.StatusUnauthorized {
		t.Fatalf("reused link: %d", code)
	}
	owner, err := a.DB.Owner(ctx)
	if err != nil || owner.Channel != "web" {
		t.Fatalf("owner: %+v %v", owner, err)
	}

	code, me := w.do("GET", "/api/me", nil, false)
	if code != 200 {
		t.Fatalf("me: %d %v", code, me)
	}
	if n := len(me["sprigs"].([]any)); n != 2 {
		t.Fatalf("sprigs: %v", me["sprigs"])
	}
	if first := me["sprigs"].([]any)[0].(map[string]any); first["id"] != "juniper" || first["host"] != true {
		t.Fatalf("host should come first: %v", first)
	}

	// A message in Pip's thread is answered by Pip, without naming her.
	pipChat := fmt.Sprintf("%d.pip", owner.ID)
	if code, _ := w.do("POST", "/api/chats/"+pipChat+"/messages", map[string]string{"text": "hello"}, false); code != http.StatusForbidden {
		t.Fatalf("post without X-Thicket: %d", code)
	}
	if code, out := w.do("POST", "/api/chats/"+pipChat+"/messages", map[string]string{"text": "hello"}, true); code != 200 {
		t.Fatalf("post: %d %v", code, out)
	}
	var msgs []store.WebMessage
	waitFor(t, "Pip's reply", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, pipChat, 0, 10)
		return len(msgs) == 2
	})
	if msgs[0].Role != "user" || msgs[1].Character != "pip" || !strings.Contains(msgs[1].Content, "Pip here") {
		t.Fatalf("pip thread: %+v", msgs)
	}
	code, out := w.do("GET", "/api/chats/"+pipChat+"/messages", nil, false)
	if code != 200 || len(out["messages"].([]any)) != 2 {
		t.Fatalf("history: %d %v", code, out)
	}

	// Pin your message; delete Pip's reply. Both reach the conversation
	// sprigs see.
	pinPath := fmt.Sprintf("/api/chats/%s/messages/%d/pin", pipChat, msgs[0].ID)
	if code, out := w.do("POST", pinPath, map[string]bool{"pinned": true}, true); code != 200 || out["pinned"] != true {
		t.Fatalf("pin: %d %v", code, out)
	}
	if _, out := w.do("GET", "/api/chats/"+pipChat+"/messages", nil, false); out["messages"].([]any)[0].(map[string]any)["pinned"] != true {
		t.Fatalf("history doesn't show the pin: %v", out)
	}
	conv, err := a.DB.CurrentConversation(ctx, owner.ID, "web:"+pipChat, a.Cfg.IdleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if pins, _ := a.DB.PinnedMessages(ctx, conv.ID); len(pins) != 1 || pins[0].Content != "hello" {
		t.Fatalf("conversation pins: %+v", pins)
	}
	if code, _ := w.do("DELETE", fmt.Sprintf("/api/chats/%s/messages/%d", fmt.Sprintf("%d.pip", owner.ID+1), msgs[1].ID), nil, true); code != http.StatusNotFound {
		t.Fatalf("deleting in someone else's chat: %d", code)
	}
	if code, out := w.do("DELETE", fmt.Sprintf("/api/chats/%s/messages/%d", pipChat, msgs[1].ID), nil, true); code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	if left, _ := a.DB.RecentMessages(ctx, conv.ID, 10); len(left) != 1 || left[0].Role != "user" {
		t.Fatalf("Pip's reply is still in the conversation: %+v", left)
	}
	if left, _ := a.DB.WebMessages(ctx, pipChat, 0, 10); len(left) != 1 {
		t.Fatalf("Pip's reply is still in the chat: %+v", left)
	}

	// In the shared room, addressing by name still routes.
	room := fmt.Sprintf("%d.all", owner.ID)
	w.do("POST", "/api/chats/"+room+"/messages", map[string]string{"text": "Pip, are you there?"}, true)
	waitFor(t, "room reply", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, room, 0, 10)
		return len(msgs) == 2 && msgs[1].Character == "pip"
	})

	// Other people's chats and unknown sprigs are off limits.
	for _, chat := range []string{fmt.Sprintf("%d.pip", owner.ID+1), fmt.Sprintf("%d.nobody", owner.ID), "garbage"} {
		if code, _ := w.do("GET", "/api/chats/"+chat+"/messages", nil, false); code != http.StatusNotFound {
			t.Fatalf("chat %s: %d", chat, code)
		}
	}

	// Failure notices reach a web-only owner in the host's thread.
	a.Engine.Notify(ctx, "test-notice", time.Hour, "error", "Something broke, heads up.")
	msgs, _ = a.DB.WebMessages(ctx, fmt.Sprintf("%d.juniper", owner.ID), 0, 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "heads up") {
		t.Fatalf("notice: %+v", msgs)
	}

	// Signing out ends the session.
	if code, _ := w.do("POST", "/api/logout", nil, true); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := w.do("GET", "/api/me", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}
}

// openStream starts GET /api/events and returns a channel that closes when
// the server ends the stream.
func openStream(t *testing.T, w *webClient) <-chan struct{} {
	t.Helper()
	req, _ := http.NewRequest("GET", w.base+"/api/events", nil)
	res, err := w.c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("events: %v %v", res, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer res.Body.Close()
		buf := make([]byte, 512)
		for {
			if _, err := res.Body.Read(buf); err != nil {
				return
			}
		}
	}()
	return done
}

func signIn(t *testing.T, a *App, ctx context.Context, base string) *webClient {
	t.Helper()
	token, err := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	w := &webClient{t: t, base: base, c: &http.Client{Jar: jar}}
	if code, out := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	return w
}

func TestWebEventStreamLifetime(t *testing.T) {
	a, _, _, ctx := setup(t)
	a.Web.PingEvery = 20 * time.Millisecond
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	defer srv.CloseClientConnections() // so a failure doesn't hang in Close

	// A stream opened before the adapter starts (App.Run starts the HTTP
	// server and the adapter separately) still ends at shutdown.
	early := openStream(t, signIn(t, a, ctx, srv.URL))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	ran := make(chan struct{})
	go func() { a.Web.Run(runCtx, a.Engine.HandleIncoming); close(ran) }()
	waitFor(t, "web adapter", a.Web.Connected)

	// Signing out in another tab ends a stream that is already open.
	w := signIn(t, a, ctx, srv.URL)
	other := &webClient{t: t, base: srv.URL, c: &http.Client{Jar: w.c.Jar}}
	done := openStream(t, w)
	other.do("POST", "/api/logout", nil, true)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream outlived its session")
	}

	// Shutting Thicket down ends open streams promptly, including the one
	// opened before the adapter started.
	w = signIn(t, a, ctx, srv.URL)
	done = openStream(t, w)
	stop()
	<-ran
	for name, ch := range map[string]<-chan struct{}{"late": done, "early": early} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s stream kept running after shutdown", name)
		}
	}
}

func TestWebLoginThrottle(t *testing.T) {
	a, _, _, ctx := setup(t)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	w := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: "203.0.113.7"}
	for i := 0; i < 10; i++ {
		if code, _ := w.do("POST", "/api/login", map[string]string{"token": fmt.Sprint("bad", i)}, true); code != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d", i, code)
		}
	}
	// Once this client's failures are used up, even a good link waits.
	token, _ := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if code, _ := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != http.StatusTooManyRequests {
		t.Fatalf("throttled login: %d", code)
	}
	// Someone else isn't locked out by it.
	other := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: "198.51.100.9"}
	if code, _ := other.do("POST", "/api/login", map[string]string{"token": "bad-other"}, true); code != http.StatusUnauthorized {
		t.Fatalf("another client was throttled: %d", code)
	}
	// A client sending a made-up X-Forwarded-For first is still counted
	// by the address its proxy appended.
	spoof := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: "192.0.2.1, 203.0.113.7"}
	if code, _ := spoof.do("POST", "/api/login", map[string]string{"token": "bad-spoof"}, true); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed header escaped the limit: %d", code)
	}
	// The link wasn't spent by the refused attempt.
	if uid, err := a.DB.RedeemWebLink(ctx, token); err != nil || uid != 0 {
		t.Fatalf("link should still work: %d %v", uid, err)
	}
	// Many addresses together still meet the household-wide backstop.
	refused := false
	for i := 0; i < 200 && !refused; i++ {
		c := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: fmt.Sprintf("10.9.%d.%d", i/200, i%200+1)}
		code, _ := c.do("POST", "/api/login", map[string]string{"token": "bad"}, true)
		refused = code == http.StatusTooManyRequests
	}
	if !refused {
		t.Fatal("no backstop across many clients")
	}
}

// A concurrent burst can't slip past the limit before failures are counted.
func TestWebLoginThrottleConcurrent(t *testing.T) {
	a, _, _, _ := setup(t)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	const n = 120
	codes := make(chan int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(fmt.Sprintf(`{"token":"bad%d"}`, i)))
			req.Header.Set("X-Thicket", "1")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			res.Body.Close()
			codes <- res.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	reached := 0
	for c := range codes {
		switch c {
		case http.StatusUnauthorized:
			reached++ // got as far as the database
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if reached > 10 {
		t.Fatalf("%d attempts reached the database; the limit is 10 per client", reached)
	}
}

// With THICKET_TRUSTED_PROXIES set, X-Forwarded-For from anywhere else
// (here: the test client on loopback) is ignored: made-up addresses all
// count as the one peer.
func TestWebLoginThrottleTrustedProxies(t *testing.T) {
	a, _, _, _ := setup(t)
	a.Web.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.99.0.0/16")}
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	for i := 0; i < 10; i++ {
		c := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: fmt.Sprintf("203.0.113.%d", i+1)}
		if code, _ := c.do("POST", "/api/login", map[string]string{"token": "bad"}, true); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	c := &webClient{t: t, base: srv.URL, c: &http.Client{}, from: "198.51.100.1"}
	if code, _ := c.do("POST", "/api/login", map[string]string{"token": "bad"}, true); code != http.StatusTooManyRequests {
		t.Fatalf("an untrusted proxy's header was believed: %d", code)
	}
}
