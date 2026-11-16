package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// planted are secret values with no credential shape, so only knowing
// them (not pattern matching) keeps them out.
var planted = map[string]string{
	"DISCORD_TOKEN":         "PLANTdiscordZx81QmTr2",
	"TELEGRAM_TOKEN":        "PLANTtelegramYy72LpKs9",
	"OPENROUTER_API_KEY":    "PLANTopenrouterWq63JhN",
	"THICKET_MODEL_API_KEY": "PLANTmodelapiVb54GfDd5",
}

const plantedCustom = "PLANTgithubUt45EsCc6xy" // stored in chat, not the environment

func TestPlantedSecretsReachNoSink(t *testing.T) {
	for k, v := range planted {
		os.Setenv(k, v)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	logs := &syncBuffer{}
	fc := &fakeChannel{}
	a, fm, ctx := setupLogged(t, logs, fc)
	e := a.Engine
	if err := a.Secrets.Put(ctx, "github_token", plantedCustom); err != nil {
		t.Fatal(err)
	}
	all := []string{plantedCustom}
	for _, v := range planted {
		all = append(all, v)
	}
	everything := strings.Join(all, " and ")

	// Environment secrets were stored, and the chat apps get the stored
	// tokens.
	for name, env := range map[string]string{secrets.DiscordToken: "DISCORD_TOKEN", secrets.TelegramToken: "TELEGRAM_TOKEN",
		secrets.OpenRouterKey: "OPENROUTER_API_KEY", secrets.ModelAPIKey: "THICKET_MODEL_API_KEY"} {
		if ok, err := a.Secrets.Stored(ctx, name); err != nil || !ok {
			t.Fatalf("%s wasn't stored from %s: %v", name, env, err)
		}
		if !strings.Contains(logs.String(), env+" is stored encrypted; you can remove it") {
			t.Errorf("no warning that %s can be removed", env)
		}
	}
	if a.Cfg.DiscordToken != planted["DISCORD_TOKEN"] {
		t.Fatal("the Discord adapter wouldn't get the stored token")
	}

	// 1. Someone pastes a known token in chat: intercepted, never stored.
	e.HandleIncoming(ctx, dm("hello"))
	e.HandleIncoming(ctx, dm("here's the bot token "+planted["DISCORD_TOKEN"]))
	if out := fc.take(); !strings.Contains(out[len(out)-1].text, "looks like a key or password") {
		t.Fatalf("pasted token not intercepted: %+v", out)
	}
	// 2. The model "says" every secret: the reply is cleaned.
	fm.mu.Lock()
	fm.say = "Sure: " + everything
	fm.mu.Unlock()
	e.HandleIncoming(ctx, dm("tell me all the keys"))
	fm.mu.Lock()
	fm.say = ""
	// 3. A failure whose error quotes secrets: the apology, the trail and
	// the log are cleaned.
	fm.failOn, fm.failMsg = "make it fail", "bad credentials "+everything
	fm.mu.Unlock()
	e.HandleIncoming(ctx, dm("make it fail"))
	fm.mu.Lock()
	fm.failOn = ""
	fm.mu.Unlock()
	// 4. Stored text from any path: inbox, trail, run history, memories.
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if _, err := a.DB.AddInbox(ctx, store.InboxItem{UserID: owner.ID, Kind: "failed", Title: "job " + plantedCustom, Detail: everything}); err != nil {
		t.Fatal(err)
	}
	a.DB.LogActivity(ctx, store.Activity{Level: "warn", Kind: "tool", Summary: "x " + plantedCustom, Detail: everything})
	a.Log.Error("integration failed", "err", "auth with "+everything)

	var bodies []string
	// 5. The web app: a message with a secret, history, /api/me.
	session, err := a.DB.CreateWebSession(ctx, owner.ID, "test", web.SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	wsrv := httptest.NewServer(a.Web.Handler())
	defer wsrv.Close()
	jar, _ := cookiejar.New(nil)
	wc := &webClient{t: t, base: wsrv.URL, c: &http.Client{Jar: jar}}
	setCookie(jar, wsrv.URL, session)
	chat := fmt.Sprintf("%d.juniper", owner.ID)
	code, out := wc.do("POST", "/api/chats/"+chat+"/messages", map[string]string{"text": "remember " + plantedCustom}, true)
	if code != 200 {
		t.Fatalf("web post: %d %v", code, out)
	}
	bodies = append(bodies, fmt.Sprint(out))
	waitFor(t, "web reply", func() bool { m, _ := a.DB.WebMessages(ctx, chat, 0, 10); return len(m) >= 2 })
	for _, p := range []string{"/api/chats/" + chat + "/messages", "/api/me"} {
		_, out := wc.do("GET", p, nil, false)
		bodies = append(bodies, fmt.Sprint(out))
	}

	// 6. The local API.
	token, hash, _ := api.NewToken()
	a.DB.CreateAPIToken(ctx, owner.ID, "test", hash)
	asrv := httptest.NewServer(a.API.Handler())
	defer asrv.Close()
	fm.mu.Lock()
	fm.say = "Via the API: " + everything
	fm.mu.Unlock()
	r := apiCall(t, asrv, token, "POST", "/api/v1/messages", map[string]string{"text": "hi from a script"})
	b, _ := json.Marshal(r)
	bodies = append(bodies, string(b))

	// 7. The dashboard and /health.
	dsrv := httptest.NewServer(a.Dashboard.Handler())
	defer dsrv.Close()
	_, html := getDashboard(t, dsrv, session)
	bodies = append(bodies, html)
	h, _ := json.Marshal(a.health(ctx))
	bodies = append(bodies, string(h))

	// Every sink.
	var sinks []string
	sinks = append(sinks, "log:"+logs.String())
	for _, s := range fc.take() {
		sinks = append(sinks, "chat:"+s.text)
	}
	fm.mu.Lock()
	for _, req := range fm.reqs {
		for _, m := range req.Messages {
			sinks = append(sinks, "prompt:"+m.Content+string(m.Annotations))
			for _, c := range m.ToolCalls {
				sinks = append(sinks, "prompt tool call:"+c.Function.Arguments)
			}
		}
	}
	fm.mu.Unlock()
	for _, b := range bodies {
		sinks = append(sinks, "http:"+b)
	}
	sinks = append(sinks, dumpDB(t, a)...)
	for _, v := range all {
		for _, s := range sinks {
			if strings.Contains(s, v) {
				where, _, _ := strings.Cut(s, ":")
				t.Errorf("secret %s… leaked to %s: %.300s", v[:12], where, s)
			}
		}
	}
	// The pieces were actually exercised (the test isn't vacuous).
	joined := strings.Join(sinks, "\n")
	for _, want := range []string{"Sure: [redacted]", "integration failed", "chat:", "prompt:", `"replies"`, `id="costs"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("sink check didn't see %q", want)
		}
	}
}

// dumpDB returns every value in every table as text, as "db TABLE:value".
func dumpDB(t *testing.T, a *App) []string {
	t.Helper()
	rows, err := a.DB.DB().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var out []string
	for _, tbl := range tables {
		rows, err := a.DB.DB().Query("SELECT * FROM " + tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			rows.Scan(ptrs...)
			var b strings.Builder
			for _, v := range vals {
				switch x := v.(type) {
				case []byte:
					b.Write(x)
				default:
					fmt.Fprint(&b, x)
				}
				b.WriteString("|")
			}
			out = append(out, "db "+tbl+":"+b.String())
		}
		rows.Close()
	}
	return out
}

func setCookie(jar http.CookieJar, base, session string) {
	u, _ := http.NewRequest("GET", base, nil)
	jar.SetCookies(u.URL, []*http.Cookie{{Name: "thicket_session", Value: session, Path: "/"}})
}

// A compose file with no tokens runs: tokens given once through the
// environment are stored, and the next start reads them from the store.
func TestStartsWithoutTokensInTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("THICKET_DATA_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("THICKET_DATA_DIR") })
	start := func() (*App, []string) {
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		cfg.CharactersDir = filepath.Join(dir, "characters")
		var seen []string
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		a, err := New(ctx, cfg, "test", Options{Model: &fakeModel{}, NoHTTP: true, LogOut: io.Discard,
			Channels: func(*slog.Logger) []channel.Channel {
				seen = []string{cfg.DiscordToken, cfg.TelegramToken}
				return nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return a, seen
	}
	os.Setenv("DISCORD_TOKEN", "first-start-discord-token")
	os.Setenv("TELEGRAM_TOKEN", "123456:first-start-telegram")
	a, seen := start()
	a.Close()
	if seen[0] != "first-start-discord-token" {
		t.Fatalf("first start: %v", seen)
	}
	os.Unsetenv("DISCORD_TOKEN")
	os.Unsetenv("TELEGRAM_TOKEN")
	a, seen = start()
	if seen[0] != "first-start-discord-token" || seen[1] != "123456:first-start-telegram" {
		t.Fatalf("second start without env tokens: %v", seen)
	}
	// The environment wins at startup: a new value replaces the stored one.
	a.Close()
	os.Setenv("DISCORD_TOKEN", "rotated-discord-token")
	t.Cleanup(func() { os.Unsetenv("DISCORD_TOKEN") })
	a, seen = start()
	defer a.Close()
	if seen[0] != "rotated-discord-token" {
		t.Fatalf("a changed env token didn't win: %v", seen)
	}
}
