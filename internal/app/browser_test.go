package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/browser"
	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/store"
)

// fakeBrowser is the thicket-browser plugin as core sees it: MCP tools
// that record what core said (_meta), and the admin API for profiles.
type fakeBrowser struct {
	mu       sync.Mutex
	calls    []browserCall
	profiles map[string][]byte
	open     map[string]bool
}

type browserCall struct {
	Tool string
	Meta struct {
		Call struct {
			Sprig string `json:"sprig"`
		} `json:"thicket/call"`
		Browser *struct {
			Allow      []string `json:"allow"`
			ProfileKey string   `json:"profile_key"`
		} `json:"thicket/browser"`
	}
}

const browserToken = "browser-token-0123456789abcdefghijklmnopqrstuvwxyz"

func (f *fakeBrowser) last(t *testing.T) browserCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the browser plugin wasn't called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeBrowser) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+browserToken {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /mcp", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string          `json:"name"`
				Meta json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"thicket-browser","version":"test"}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[`+
				`{"name":"open","description":"open","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},`+
				`{"name":"click","description":"click","inputSchema":{"type":"object"}}]}}`, *req.ID)
		case "tools/call":
			c := browserCall{Tool: req.Params.Name}
			_ = json.Unmarshal(req.Params.Meta, &c.Meta)
			f.mu.Lock()
			f.calls = append(f.calls, c)
			f.mu.Unlock()
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"Page (untrusted content from shop.example)"}]}}`, *req.ID)
		}
	}))
	mux.HandleFunc("GET /v1/doctor", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"checks":[{"name":"chromium","ok":true,"detail":"Chromium 1 started"},{"name":"proxy_blocks_unlisted","ok":true,"detail":"refused"}],"open":[],"max":2}`)
	}))
	mux.HandleFunc("PUT /v1/profiles/{sprig}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.open[r.PathValue("sprig")] {
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"open"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.profiles[r.PathValue("sprig")] = b
		io.WriteString(w, `{}`)
	}))
	return mux
}

const wrenBrowser = `---
id: wren
name: Wren
role: Handles signups and receipts.
personality:
  traits: [tidy]
voice:
  style: [short]
household: solo
belongs_to: 1
tools: ["mcp:browser/*"]
---
Wren keeps the household's paperwork.
`

const fernBrowser = `---
id: fern
name: Fern
role: The household's errand runner.
personality:
  traits: [quick]
voice:
  style: [short]
household: family
tools: ["mcp:browser/*"]
---
Fern runs errands.
`

// browserReady is a household with the browser plugin connected as
// Thicket sets it up, Wren (the owner's private sprig) and Fern
// (shared), both granted the browser.
func browserReady(t *testing.T) (*App, *fakeChannel, context.Context, *fakeBrowser, *store.User, *httptest.Server) {
	t.Helper()
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	for name, body := range map[string]string{"wren.md": wrenBrowser, "fern.md": fernBrowser} {
		if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBrowser{profiles: map[string][]byte{}, open: map[string]bool{}}
	srv := httptest.NewServer(fb.handler())
	t.Cleanup(srv.Close)
	if err := a.Secrets.Put(ctx, "browser_token", browserToken); err != nil {
		t.Fatal(err)
	}
	s := store.MCPServer{Name: "browser", Transport: "http", Target: srv.URL + "/mcp", Enabled: true,
		Headers: map[string]string{"Authorization": "Bearer {{secret:browser_token}}"}}
	if err := a.DB.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MCP.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	a.browser = &browser.Client{URL: srv.URL, Token: func(context.Context) (string, error) { return browserToken, nil }}
	a.Dashboard.Browser = a.Engine
	fc.take()
	return a, fc, ctx, fb, owner, srv
}

func TestBrowserCallsCarryAllowlistAndKey(t *testing.T) {
	a, fc, ctx, fb, owner := func() (*App, *fakeChannel, context.Context, *fakeBrowser, *store.User) {
		a, fc, ctx, fb, owner, _ := browserReady(t)
		return a, fc, ctx, fb, owner
	}()
	e := a.Engine
	list, err := e.ChangeBrowserAllow(ctx, owner, "wren", []string{"https://www.Shop.example/cart?x=1", "*.bank.example", "bank.example"}, nil, "test")
	if err != nil || strings.Join(list, ",") != "bank.example,www.shop.example" {
		t.Fatalf("%v %v", list, err)
	}
	e.HandleIncoming(ctx, dm(`wren, call tool mcp_browser__open {"url":"https://www.shop.example/"}`))
	fc.take()
	c := fb.last(t)
	if c.Tool != "open" || c.Meta.Call.Sprig != "wren" || c.Meta.Browser == nil || strings.Join(c.Meta.Browser.Allow, ",") != "bank.example,www.shop.example" {
		t.Fatalf("call: %+v %+v", c, c.Meta.Browser)
	}
	key, err := base64.StdEncoding.DecodeString(c.Meta.Browser.ProfileKey)
	if err != nil || len(key) != 32 {
		t.Fatalf("profile key %q: %v", c.Meta.Browser.ProfileKey, err)
	}
	if stored, _ := a.Secrets.Get(ctx, browser.ProfileKeySecret("wren")); stored != c.Meta.Browser.ProfileKey {
		t.Fatal("the profile key isn't the one in the secret store")
	}

	// A removal counts at the next call; the key stays the same.
	if _, err := e.ChangeBrowserAllow(ctx, owner, "wren", nil, []string{"bank.example"}, "test"); err != nil {
		t.Fatal(err)
	}
	e.HandleIncoming(ctx, dm(`wren, call tool mcp_browser__open {"url":"https://bank.example/"}`))
	fc.take()
	c2 := fb.last(t)
	if strings.Join(c2.Meta.Browser.Allow, ",") != "www.shop.example" || c2.Meta.Browser.ProfileKey != c.Meta.Browser.ProfileKey {
		t.Fatalf("second call: %+v", c2.Meta.Browser)
	}

	// Another sprig has its own key and its own (empty) list.
	e.HandleIncoming(ctx, dm(`fern, call tool mcp_browser__open {"url":"https://www.shop.example/"}`))
	fc.take()
	c3 := fb.last(t)
	if c3.Meta.Call.Sprig != "fern" || len(c3.Meta.Browser.Allow) != 0 || c3.Meta.Browser.ProfileKey == c.Meta.Browser.ProfileKey {
		t.Fatalf("fern's call: %+v", c3.Meta.Browser)
	}
	// The key never reaches the trail or the chat.
	for _, tr := range mustTrail(t, a, ctx) {
		if strings.Contains(tr.Summary+tr.Detail, c.Meta.Browser.ProfileKey) {
			t.Fatal("the profile key is in the trail")
		}
	}
}

func TestBrowserAllowlistOwnership(t *testing.T) {
	a, fc, ctx, _, owner, _ := browserReady(t)
	e := a.Engine
	kim, _ := a.DB.CreateUser(ctx, "discord", "kim", "Kim", "member")
	kid, _ := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	for _, bad := range []string{"192.168.1.1", "http://10.0.0.5/", "localhost", "nas", "router.lan", "printer.local", "metadata.google.internal", "*", "not a site", "[::1]", "bücher.example"} {
		if _, err := e.ChangeBrowserAllow(ctx, owner, "fern", []string{bad}, nil, "test"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := e.ChangeBrowserAllow(ctx, owner, "fern", []string{"Shop.Example", "store.example:443"}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ChangeBrowserAllow(ctx, kim, "fern", []string{"kim.example"}, nil, "test"); err == nil {
		t.Fatal("a member changed a shared sprig's browser allowlist")
	}
	if l, err := e.BrowserAllowlist(ctx, kim, "fern"); err != nil || strings.Join(l, ",") != "shop.example,store.example" {
		t.Fatalf("a member can't see a shared sprig's allowlist: %v %v", l, err)
	}
	if _, err := e.BrowserAllowlist(ctx, kid, "fern"); err == nil {
		t.Fatal("a kid read a sprig's browser allowlist")
	}
	if _, err := e.BrowserAllowlist(ctx, kim, "wren"); err == nil {
		t.Fatal("someone read another person's private sprig's allowlist")
	}
	if _, err := e.ChangeBrowserAllow(ctx, kim, "wren", []string{"kim.example"}, nil, "test"); err == nil || strings.Contains(err.Error(), "Wren") {
		t.Fatalf("someone changed (or learned of) another person's private sprig: %v", err)
	}

	// In chat: the owner's tool shows and changes it.
	a.Engine.HandleIncoming(ctx, dm(`call tool browser_allowlist {"sprig":"fern","add":["library.example"]}`))
	fc.take()
	if l, _ := a.DB.BrowserAllow(ctx, "fern"); strings.Join(l, ",") != "library.example,shop.example,store.example" {
		t.Fatalf("after the chat tool: %v", l)
	}

	// Every change is in the trail; only the latest is undone.
	var first *store.TrailEntry
	for _, tr := range mustTrail(t, a, ctx) {
		if strings.Contains(tr.Summary, "Fern's browser allowlist: added shop.example, store.example") {
			tr := tr
			first = &tr
		}
	}
	if first == nil || !first.Undoable {
		t.Fatal("no undoable trail entry")
	}
	if _, err := e.UndoTrail(ctx, kim, first.ID); err == nil {
		t.Fatal("a member undid a browser allowlist change on a shared sprig")
	}
	if _, err := e.UndoTrail(ctx, owner, first.ID); err == nil || !strings.Contains(err.Error(), "changed again") {
		t.Fatalf("an older change was undone over a newer one: %v", err)
	}
	newer, _ := a.DB.TrailOfKind(ctx, store.TrailAccess, "fern", 1)
	if _, err := e.UndoTrail(ctx, owner, newer[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.UndoTrail(ctx, owner, first.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := a.DB.BrowserAllow(ctx, "fern"); len(l) != 0 {
		t.Fatalf("undo left %v", l)
	}
	// The list isn't a setting anyone can change on the settings screen.
	if _, err := e.AdminSetSetting(ctx, owner, "browser.allow.fern", `["evil.example"]`); err == nil {
		t.Fatal("the allowlist was changed as a setting")
	}
}

func TestBrowserKidSprigsHaveNoBrowser(t *testing.T) {
	a, fc, ctx, _, owner, _ := browserReady(t)
	kid, _ := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	body := fmt.Sprintf("---\nid: moss\nname: Moss\nrole: A reading buddy.\npersonality:\n  traits: [kind]\nvoice:\n  style: [gentle]\nhousehold: kid\nkids: [%d]\nparents: [%d]\n---\nMoss reads.\n", kid.ID, owner.ID)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "moss.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, dm(`call tool grant_tools {"character":"moss","add":["mcp:browser/*"]}`))
	fc.take()
	if c, _ := a.Chars.Get("moss"); len(c.Tools) != 0 {
		t.Fatalf("a kid sprig was granted the browser: %v", c.Tools)
	}
	if _, err := a.Engine.ChangeBrowserAllow(ctx, owner, "moss", []string{"shop.example"}, nil, "test"); err == nil {
		t.Fatal("a kid sprig got a browser allowlist")
	}
}

func TestBrowserOnTheDashboardAndAPI(t *testing.T) {
	a, _, ctx, _, owner, _ := browserReady(t)
	kim, _ := a.DB.CreateUser(ctx, "discord", "kim", "Kim", "member")
	kid, _ := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session := func(u *store.User) string {
		s, err := a.DB.CreateWebSession(ctx, u.ID, "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(sess, method, path string, form url.Values) (int, string) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequest(method, srv.URL+path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: sess})
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	own, mem, child := session(owner), session(kim), session(kid)

	code, body := do(own, "GET", "/dashboard/browser", nil)
	if code != 200 || !strings.Contains(body, "Wren") || !strings.Contains(body, "Fern") || !strings.Contains(body, `value="allow"`) {
		t.Fatalf("owner's page %d:\n%s", code, body)
	}
	if _, main := do(own, "GET", "/dashboard", nil); !strings.Contains(main, `href="/dashboard/browser"`) {
		t.Fatal("no Browser link on the dashboard")
	}
	code, body = do(mem, "GET", "/dashboard/browser", nil)
	if code != 200 || strings.Contains(body, "Wren") || !strings.Contains(body, "Fern") || strings.Contains(body, `value="allow"`) {
		t.Fatalf("member's page %d:\n%s", code, body)
	}
	if _, body = do(child, "GET", "/dashboard/browser", nil); strings.Contains(body, "Fern") {
		t.Fatal("a kid saw a sprig's browser")
	}
	// The owner allows a site; a member's post is refused; a stale form too.
	code, _ = do(own, "POST", "/dashboard/browser/act", url.Values{"csrf": {dashboard.CSRFToken(own)}, "sprig": {"fern"}, "entry": {"https://shop.example/"}, "action": {"allow"}})
	if code != http.StatusSeeOther {
		t.Fatalf("allow: %d", code)
	}
	if l, _ := a.DB.BrowserAllow(ctx, "fern"); strings.Join(l, ",") != "shop.example" {
		t.Fatalf("after allow: %v", l)
	}
	if code, _ = do(mem, "POST", "/dashboard/browser/act", url.Values{"csrf": {dashboard.CSRFToken(mem)}, "sprig": {"fern"}, "entry": {"evil.example"}, "action": {"allow"}}); code != 400 {
		t.Fatalf("member allow: %d", code)
	}
	if code, _ = do(mem, "POST", "/dashboard/browser/act", url.Values{"csrf": {dashboard.CSRFToken(mem)}, "sprig": {"wren"}, "entry": {"evil.example"}, "action": {"allow"}}); code != 404 {
		t.Fatalf("member on a private sprig: %d", code)
	}
	if code, _ = do(own, "POST", "/dashboard/browser/act", url.Values{"csrf": {"stale"}, "sprig": {"fern"}, "entry": {"evil.example"}, "action": {"allow"}}); code != 403 {
		t.Fatalf("stale form: %d", code)
	}

	// API v2.
	for u, name := range map[*store.User]string{owner: "sam", kim: "kim"} {
		hash, _ := auth.HashPassword(name + " long password")
		if err := a.DB.SetLogin(ctx, u.ID, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	api := httptest.NewServer(a.Web.V2Handler())
	defer api.Close()
	anon := &v2client{t: t, base: api.URL}
	login := func(user string) *v2client {
		code, out := anon.call("POST", "/api/v2/auth/login", map[string]string{"username": user, "password": user + " long password"})
		if code != 200 {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return &v2client{t: t, base: api.URL, token: out["token"].(string)}
	}
	sam, kimc := login("sam"), login("kim")
	if code, out := sam.call("PATCH", "/api/v2/sprigs/fern/browser/allowlist", map[string]any{"add": []string{"library.example"}, "remove": []string{"shop.example"}}); code != 200 || fmt.Sprint(out["allowlist"]) != "[library.example]" {
		t.Fatalf("owner PATCH: %d %v", code, out)
	}
	if code, _ := kimc.call("PATCH", "/api/v2/sprigs/fern/browser/allowlist", map[string]any{"add": []string{"evil.example"}}); code != 400 {
		t.Fatalf("member PATCH: %d", code)
	}
	if code, out := kimc.call("GET", "/api/v2/sprigs/fern/browser/allowlist", nil); code != 200 || fmt.Sprint(out["allowlist"]) != "[library.example]" {
		t.Fatalf("member GET: %d %v", code, out)
	}
	if code, _ := kimc.call("GET", "/api/v2/sprigs/wren/browser/allowlist", nil); code != 404 {
		t.Fatalf("member GET of a private sprig: %d", code)
	}
}

func TestBrowserRestoredProfilesGoBackToThePlugin(t *testing.T) {
	a, _, ctx, fb, _, _ := browserReady(t)
	old := profilePushRetry
	profilePushRetry = 20 * time.Millisecond
	defer func() { profilePushRetry = old }()

	// A backup with a sealed profile, restored into a fresh data folder.
	var buf bytes.Buffer
	sealed := append([]byte("TBP1"), bytes.Repeat([]byte{7}, 60)...)
	if err := backup.ExportWith(ctx, a.DB, a.Cfg.DataDir, a.Cfg.CharactersDir, false, []backup.BrowserProfile{{Sprig: "wren", Sealed: sealed}}, &buf); err != nil {
		t.Fatal(err)
	}
	if err := backup.ExportWith(ctx, a.DB, a.Cfg.DataDir, a.Cfg.CharactersDir, false, []backup.BrowserProfile{{Sprig: "../x", Sealed: sealed}}, io.Discard); err == nil {
		t.Fatal("a profile with a bad name was exported")
	}
	fresh := t.TempDir()
	if err := backup.Restore(bytes.NewReader(buf.Bytes()), fresh, filepath.Join(fresh, "characters"), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(backup.BrowserProfilesDir(fresh), "wren.tbp"))
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatalf("restored profile: %v", err)
	}

	// Thicket hands it to the plugin: not while that browser is open,
	// then once it closes, and deletes its copy.
	if err := os.MkdirAll(backup.BrowserProfilesDir(a.Cfg.DataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(backup.BrowserProfilesDir(a.Cfg.DataDir), "wren.tbp")
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	fb.open["wren"] = true
	fb.mu.Unlock()
	done := make(chan struct{})
	go func() { a.pushRestoredProfiles(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the profile was dropped while the plugin couldn't take it")
	}
	fb.mu.Lock()
	fb.open["wren"] = false
	fb.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never handed over")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !bytes.Equal(fb.profiles["wren"], sealed) {
		t.Fatal("the plugin didn't get the profile")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the restored copy wasn't deleted")
	}
}
