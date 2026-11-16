package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
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
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	a.Web.PingEvery = 20 * time.Millisecond
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()

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

	// Shutting Thicket down ends open streams promptly.
	w = signIn(t, a, ctx, srv.URL)
	done = openStream(t, w)
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream kept running after shutdown")
	}
}

func TestWebLoginThrottle(t *testing.T) {
	a, _, _, ctx := setup(t)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	w := &webClient{t: t, base: srv.URL, c: &http.Client{}}
	for i := 0; i < 30; i++ {
		if code, _ := w.do("POST", "/api/login", map[string]string{"token": fmt.Sprint("bad", i)}, true); code != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d", i, code)
		}
	}
	// Once the window's failures are used up, even a good link waits.
	token, _ := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if code, _ := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != http.StatusTooManyRequests {
		t.Fatalf("throttled login: %d", code)
	}
	// The link wasn't spent by the refused attempt.
	if uid, err := a.DB.RedeemWebLink(ctx, token); err != nil || uid != 0 {
		t.Fatalf("link should still work: %d %v", uid, err)
	}
}
