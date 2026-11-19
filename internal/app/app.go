// Package app wires Thicket together. cmd/thicket and the smoke test both
// build the whole system through here.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/server"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

type Options struct {
	// Model overrides the OpenRouter client (tests).
	Model llm.Client
	// KeyCheck overrides OpenRouter key validation (tests).
	KeyCheck func(ctx context.Context, key string) error
	// Channels builds the chat adapters with Thicket's (redacting) logger.
	Channels func(log *slog.Logger) []channel.Channel
	// NewChannel builds the adapter for a chat app ("discord", "telegram")
	// from its bot token; nil for an app it doesn't know. With it set,
	// Thicket starts the apps it has tokens for, and can connect one while
	// it runs (a token sent in chat).
	NewChannel func(name, token string, log *slog.Logger) channel.Channel
	LogOut     io.Writer
	// NoHTTP skips the HTTP server.
	NoHTTP bool
}

type App struct {
	Cfg       *config.Config
	Version   string
	Log       *slog.Logger
	DB        *store.Store
	Secrets   *secrets.Store
	Settings  *settings.Settings
	Chars     *character.Registry
	Guard     *budget.Guard
	MCP       *mcp.Registry
	OAuth     *mcp.OAuth
	Calendar  *calendar.Service
	Engine    *agent.Engine
	Reviewer  *memory.Reviewer
	Scheduler *scheduler.Scheduler
	API       *api.API
	Dashboard *dashboard.Dashboard
	HTTP      *http.Server
	Web       *web.Adapter

	newChannel func(name, token string, log *slog.Logger) channel.Channel
	connectMu  sync.Mutex // one chat app connects at a time

	mu        sync.Mutex
	startErrs []string
	// backupErr is why the last scheduled backup failed ("" if it worked).
	backupErr string
	channels  []channel.Channel    // chat adapters, the web app included
	live      map[string]*liveChan // the running ones, by name
	startTok  map[string]string    // tokens of the apps built at startup
	runCtx    context.Context      // set by Run
	stopping  bool
	wg        sync.WaitGroup
}

// goRun runs a background task that Run waits for. A crash is reported,
// not fatal.
func (a *App) goRun(name string, f func()) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				a.Log.Error("background task crashed", "task", name, "panic", p)
				a.Engine.Notify(context.Background(), "crash:"+name, time.Hour, "error",
					fmt.Sprintf("Part of me (%s) crashed: %v. Restarting Thicket should fix it; the logs have details.", name, p))
			}
		}()
		f()
	}()
}

func (a *App) startErr(msg string) {
	a.mu.Lock()
	a.startErrs = append(a.startErrs, msg)
	a.mu.Unlock()
}

func NewLogger(level string, out io.Writer, red *secrets.Redactor) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	h := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lv})
	return slog.New(secrets.NewLogHandler(h, red))
}

func New(ctx context.Context, cfg *config.Config, version string, opts Options) (*App, error) {
	red := secrets.NewRedactor()
	// Chat tokens aren't stored secrets, but must never be logged either.
	red.Add(cfg.DiscordToken)
	red.Add(cfg.TelegramToken)
	out := opts.LogOut
	if out == nil {
		out = os.Stdout
	}
	log := NewLogger(cfg.LogLevel, out, red)
	a := &App{Cfg: cfg, Version: version, Log: log, newChannel: opts.NewChannel,
		live: map[string]*liveChan{}, startTok: map[string]string{}}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	a.DB = db
	if err := db.SetSetting(ctx, store.VersionKey, version); err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	// Whatever is written to the trail, conversations, the inbox and run
	// history passes through the redactor first.
	db.SetScrubber(red.Redact)

	key, _, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	sec, err := secrets.New(db, key, red)
	if err != nil {
		return nil, err
	}
	if err := sec.WarmRedactor(ctx); err != nil {
		a.startErr("Some stored secrets couldn't be decrypted (" + err.Error() + "). Did THICKET_SECRET_KEY, THICKET_SECRET_KEY_FILE or secret.key change?")
		log.Error("secrets", "err", err)
	}
	a.Secrets = sec
	a.importEnvSecrets(ctx)
	// Chat apps start with the stored tokens (the environment's, if it
	// still has them, were just stored).
	cfg.DiscordToken = a.storedToken(ctx, secrets.DiscordToken, cfg.DiscordToken)
	cfg.TelegramToken = a.storedToken(ctx, secrets.TelegramToken, cfg.TelegramToken)
	if opts.Channels != nil {
		a.channels = opts.Channels(log)
	}
	if opts.NewChannel != nil {
		for _, t := range []struct{ name, token string }{{"discord", cfg.DiscordToken}, {"telegram", cfg.TelegramToken}} {
			if t.token == "" {
				continue
			}
			if ch := opts.NewChannel(t.name, t.token, log); ch != nil {
				a.channels = append(a.channels, ch)
				a.startTok[t.name] = t.token
			}
		}
	}
	a.Settings = settings.New(db, cfg)

	a.Chars = character.NewRegistry(cfg.CharactersDir, db, log)
	a.Settings.CharacterExists = func(id string) bool { _, ok := a.Chars.Get(id); return ok }
	if err := a.Chars.EnsureDefaults(ctx); err != nil {
		return nil, fmt.Errorf("characters: %w", err)
	}
	if err := a.Chars.Load(); err != nil {
		return nil, fmt.Errorf("characters: %w", err)
	}
	for _, le := range a.Chars.Errors() {
		a.startErr("A character file has problems, so I skipped it: " + le.Error())
	}
	if _, ok := a.Chars.Host(); !ok {
		return nil, errors.New("no host character could be loaded; fix or delete the files in " + cfg.CharactersDir + " (Juniper is recreated if juniper.md is missing)")
	}

	// The address and key are read per call: onboarding can switch to a
	// local server, and keys added in chat apply at once.
	or := llm.NewOpenRouter(cfg.ModelURL, func(ctx context.Context, base string) (string, error) {
		if llm.IsOpenRouter(base) {
			return sec.Get(ctx, secrets.OpenRouterKey)
		}
		k, _ := sec.Get(ctx, secrets.ModelAPIKey) // most local servers need none
		return k, nil
	})
	or.URL = func(ctx context.Context) string { return a.Settings.String(ctx, "model_url") }
	var base llm.Client = or
	keyCheck := or.CheckKey
	if opts.Model != nil {
		base = opts.Model
	}
	if opts.KeyCheck != nil {
		keyCheck = opts.KeyCheck
	}

	a.Engine = &agent.Engine{Cfg: cfg, DB: db, Chars: a.Chars, Settings: a.Settings, Secrets: sec, Log: log, KeyCheck: keyCheck}
	if opts.NewChannel != nil {
		a.Engine.ConnectChat = a.ConnectChat
	}
	router := &llm.Router{
		Client:   base,
		Primary:  func(ctx context.Context) string { return a.Settings.String(ctx, "model") },
		Fallback: func(ctx context.Context) string { return a.Settings.String(ctx, "fallback_model") },
		Log:      log,
		Retries:  2,
		OnFallback: func(ctx context.Context, from, to string, cause error) {
			a.Engine.Notify(ctx, "fallback:"+from, 6*time.Hour, "warn",
				fmt.Sprintf("Small heads-up: the main model (%s) is having trouble (%s), so I'm using %s for now.", from, shortErr(cause), to))
		},
	}
	if opts.Model != nil {
		router.Backoff = time.Millisecond
	}
	a.Guard = &budget.Guard{Next: redactingModel{next: router, red: red}, DB: db, Settings: a.Settings, Log: log,
		OnAlert: func(ctx context.Context, al budget.Alert) {
			a.Engine.Notify(ctx, al.Key, 31*24*time.Hour, al.Level, al.Message)
		}}
	// Spoken replies go to speech_url, or the model server when it's
	// empty, with that server's key; the guard meters them.
	a.Guard.Voice = &llm.SpeechClient{HTTP: &http.Client{}, Key: or.Key, URL: func(ctx context.Context) string {
		if u := a.Settings.String(ctx, "speech_url"); u != "" {
			return u
		}
		return a.Settings.String(ctx, "model_url")
	}}
	a.Engine.LLM = a.Guard
	a.Engine.Budget = a.Guard
	a.Guard.Paused = a.Engine.Paused // the kill switch, under every model call
	a.Engine.Speech = a.Guard

	a.MCP = mcp.NewRegistry(db, sec, log)
	// Local (stdio) integrations run one process per sprig, in its own
	// folder; listing their tools uses a neutral folder. They share a HOME
	// for caches, outside every sprig's folder.
	a.MCP.Home = filepath.Join(cfg.DataDir, "mcp", "home")
	a.MCP.Folder = func(sprig string) (string, error) {
		dir := filepath.Join(cfg.DataDir, "mcp", "work")
		if sprig != "" {
			dir = filepath.Join(a.Chars.Dir(), sprig)
		}
		for _, d := range []string{dir, a.MCP.Home} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return "", err
			}
		}
		return filepath.Abs(dir)
	}
	a.OAuth = &mcp.OAuth{Secrets: sec, RedirectURL: cfg.WebBaseURL() + "/oauth/callback"}
	a.MCP.OAuth = a.OAuth
	a.Engine.MCP = a.MCP
	a.Engine.OAuth = a.OAuth
	a.Calendar = &calendar.Service{DB: db, Secrets: sec}
	a.Engine.Calendar = a.Calendar
	a.Web = web.New(db, a.Chars, log)
	a.Web.SecureCookies = strings.HasPrefix(cfg.PublicURL, "https://")
	a.Web.Status = a.webStatus
	a.Web.Pin = a.Engine.PinWebMessage
	a.Web.Remember = a.Engine.RememberWebMessage
	a.Web.OIDC = a.oidcLogin(ctx)
	a.Web.Household = a.Engine
	a.Web.Admin = a.Engine
	a.Chars.OnLoad(a.Web.RosterChanged)
	a.Web.Calendar = a.Calendar
	a.Web.BaseURL = cfg.WebBaseURL()
	a.Web.Joined = a.Engine.WebJoined
	a.Web.MakeKidSprig = func(ctx context.Context, by *store.User, body json.RawMessage) (*character.Character, error) {
		var spec agent.KidSprigSpec
		if err := json.Unmarshal(body, &spec); err != nil {
			return nil, fmt.Errorf("bad kid sprig: %v", err)
		}
		return a.Engine.CreateKidSprig(ctx, by, spec)
	}
	a.channels = append(a.channels, a.Web)
	for _, ch := range a.channels {
		a.Engine.AddChannel(ch)
	}

	a.Reviewer = &memory.Reviewer{DB: db, LLM: a.Guard, Settings: a.Settings, Chars: a.Chars, Log: log, Idle: cfg.IdleAfter,
		OnFailure: func(ctx context.Context, userID int64, err error) {
			a.Engine.Notify(ctx, "memory-review", 6*time.Hour, "error",
				"My memory review keeps failing ("+shortErr(err)+"). Nothing is lost, but I'm not learning from recent chats until it's fixed.")
		}}
	a.Scheduler = scheduler.New(db, a.Engine, log)
	a.Engine.Fire = a.Scheduler.Fire
	// The local API is an internal channel: replies to its messages come
	// back through it. It isn't listed in /health like the chat apps.
	a.API = api.New(a.Engine, db, a.Scheduler, a.Chars, log)
	a.Engine.AddChannel(a.API)
	a.Dashboard = &dashboard.Dashboard{DB: db, Chars: a.Chars, Settings: a.Settings, Version: version, Log: log,
		States: a.Engine.SprigStates, Decide: a.Engine.DecideApproval, RunDraft: a.Engine.RunDraft, Undo: a.Engine.UndoTrail,
		Pause: func(ctx context.Context, u *store.User, sprig string, on bool) error {
			if on {
				return a.Engine.Pause(ctx, u, sprig, "from the dashboard")
			}
			return a.Engine.Resume(ctx, u, sprig, "from the dashboard")
		},
		SessionTTL: web.SessionTTL, BaseURL: cfg.WebBaseURL(), Device: web.Device,
		Health: func(ctx context.Context) map[string]dashboard.Check {
			out := map[string]dashboard.Check{}
			for k, c := range a.health(ctx) {
				out[k] = dashboard.Check{OK: c.OK, Detail: c.Detail}
			}
			return out
		}}

	if !opts.NoHTTP {
		a.HTTP = server.New(cfg.HTTPAddr, version, cfg.CharactersDir, a.health, server.Routes{
			Hooks: a.Webhook, MaxHookBody: agent.MaxWebhookPayload,
			API: a.API.Handler(), Dashboard: a.Dashboard.Handler(), App: a.Web.Handler(),
			Avatar: a.Avatar, OAuthCallback: http.HandlerFunc(a.oauthCallback), V2: a.Web.V2Handler(),
		})
	}
	return a, nil
}

// importEnvSecrets stores keys and tokens given in the environment, so
// they can be taken out of the compose file.
func (a *App) importEnvSecrets(ctx context.Context) {
	var items []secrets.EnvSecret
	envOf := map[string]string{}
	for _, e := range a.Cfg.EnvSecrets() {
		items = append(items, secrets.EnvSecret{Name: e.Name, Env: e.Env, Value: e.Value})
		envOf[e.Name] = e.Env
	}
	changed, err := a.Secrets.ImportEnv(ctx, items)
	if err != nil {
		a.Log.Error("couldn't store secrets from the environment; using them from there", "err", err)
	}
	for _, n := range changed {
		a.Log.Info("stored "+envOf[n]+" encrypted", "secret", n)
	}
	for _, it := range items {
		if strings.TrimSpace(it.Value) != "" {
			a.Log.Warn(it.Env+" is stored encrypted; you can remove it from your compose file or environment (Thicket reads it from the store from now on, and a value set there still wins at startup)", "secret", it.Name)
		}
	}
}

// oidcLogin sets up single sign-on if the issuer, client id and secret
// are stored (from THICKET_OIDC_* or `thicket secrets set oidc_...`).
func (a *App) oidcLogin(ctx context.Context) *web.OIDCLogin {
	get := func(n string) string { v, _ := a.Secrets.Get(ctx, n); return strings.TrimSpace(v) }
	iss, id, secret := strings.TrimRight(get("oidc_issuer"), "/"), get("oidc_client_id"), get("oidc_client_secret")
	if iss == "" && id == "" && secret == "" {
		return nil
	}
	if iss == "" || id == "" || secret == "" {
		a.startErr("Single sign-on is half set up: it needs the issuer, client id and client secret (THICKET_OIDC_ISSUER, _CLIENT_ID, _CLIENT_SECRET).")
		return nil
	}
	if !strings.HasPrefix(a.Cfg.PublicURL, "https://") && !strings.HasPrefix(a.Cfg.PublicURL, "http://") {
		a.startErr("Single sign-on needs THICKET_PUBLIC_URL (the address people open Thicket at), for the provider to send them back to.")
		return nil
	}
	c := a.Cfg
	gated := c.OIDCAdminGroup != "" || c.OIDCMemberGroup != ""
	scopes := c.OIDCScopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
		if gated {
			scopes = append(scopes, "groups")
		}
	}
	var wanted []string
	if gated {
		wanted = []string{c.OIDCGroupsClaim}
	}
	name := c.OIDCName
	if name == "" {
		name = "single sign-on"
	}
	a.Log.Info("single sign-on is on", "issuer", iss)
	return &web.OIDCLogin{
		Provider: auth.NewProvider(auth.OIDCConfig{Issuer: iss, ClientID: id, ClientSecret: secret,
			RedirectURL: c.PublicURL + "/auth/oidc/callback", Scopes: scopes, Wanted: wanted}),
		Name: name, GroupsClaim: c.OIDCGroupsClaim, AdminGroup: c.OIDCAdminGroup, MemberGroup: c.OIDCMemberGroup,
	}
}

// storedToken reads a chat token from the store, falling back to the
// environment's value.
func (a *App) storedToken(ctx context.Context, name, env string) string {
	v, err := a.Secrets.Get(ctx, name)
	if err != nil {
		if !errors.Is(err, secrets.ErrNotFound) {
			a.startErr("The stored " + name + " can't be read (" + err.Error() + ").")
		}
		return env
	}
	return v
}

// Avatar draws a sprig's avatar; state "" means what it's doing now.
func (a *App) Avatar(ctx context.Context, id, state string) (string, bool) {
	c, ok := a.Chars.Get(id)
	if !ok {
		return "", false
	}
	if state == "" {
		state = a.Engine.SprigStates(ctx)[c.ID]
	}
	return avatar.Render(avatar.Load(a.Chars.Dir(), c.ID, c.Color), state), true
}

func shortErr(err error) string {
	if err == nil {
		return "unknown error"
	}
	s := err.Error()
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

func (a *App) health(ctx context.Context) map[string]server.Check {
	out := map[string]server.Check{}
	if err := a.DB.Ping(ctx); err != nil {
		out["database"] = server.Check{OK: false, Detail: err.Error()}
	} else {
		out["database"] = server.Check{OK: true}
	}
	for _, ch := range a.chanList() {
		if ch == channel.Channel(a.Web) {
			continue // the web app is up whenever /health answers
		}
		out["channel."+ch.Name()] = server.Check{OK: ch.Connected()}
	}
	switch url := a.Settings.String(ctx, "model_url"); {
	case a.Engine.ModelReady(ctx) && llm.IsOpenRouter(url):
		out["model_key"] = server.Check{OK: true}
	case a.Engine.ModelReady(ctx):
		out["model_key"] = server.Check{OK: true, Detail: "model server " + url}
	default:
		out["model_key"] = server.Check{OK: false, Detail: "no model yet: DM the bot to set one up"}
	}
	last := a.Scheduler.LastTick()
	out["scheduler"] = server.Check{OK: !last.IsZero() && time.Since(last) < 2*time.Minute, Detail: "last tick " + last.UTC().Format(time.RFC3339)}
	host, ok := a.Chars.Host()
	detail := fmt.Sprintf("%d loaded", len(a.Chars.All()))
	if n := len(a.Chars.Errors()); n > 0 {
		detail += fmt.Sprintf(", %d file(s) with problems", n)
	}
	out["characters"] = server.Check{OK: ok && host != nil, Detail: detail}
	// /health is public: connection errors can quote URLs and tokens, so
	// only the state is shown. `thicket doctor` gives the details.
	for name, st := range a.MCP.Status() {
		c := server.Check{OK: !strings.HasPrefix(st, "error"), Detail: st, Optional: true}
		if !c.OK {
			c.Detail = "not connected (run thicket doctor for details)"
		}
		out["mcp."+name] = c
	}
	out["backup"] = a.backupCheck(ctx)
	for k, c := range out { // /health is public
		c.Detail = a.Secrets.Redact(c.Detail)
		out[k] = c
	}
	return out
}

func (a *App) webStatus(ctx context.Context, admin bool) web.Status {
	st := web.Status{Channels: map[string]bool{}, ModelKey: a.Engine.ModelReady(ctx)}
	for _, ch := range a.chanList() {
		st.Channels[ch.Name()] = ch.Connected()
	}
	if admin {
		now := time.Now()
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		mon := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		st.SpendDay, _ = a.DB.SpendSince(ctx, day, "")
		st.SpendMon, _ = a.DB.SpendSince(ctx, mon, "")
		st.BudgetDay = a.Settings.Float(ctx, "budget.daily_usd")
		st.BudgetMon = a.Settings.Float(ctx, "budget.monthly_usd")
	}
	return st
}

// Run starts everything and blocks until ctx is cancelled.
func (a *App) Run(parent context.Context) error {
	// Workers get a child context so every exit path stops them, and Run
	// waits for them before returning (Close must not race live workers).
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	a.Engine.SetLifetime(ctx) // background tasks end with Thicket
	// Before anything can start a new task: fail the ones a stop or crash
	// cut short (reported once the chat apps are up).
	interrupted := a.Engine.RecoverInterrupted(ctx)
	a.Scheduler.RecoverStale(ctx)
	startChans := a.chanList()
	errc := make(chan error, len(startChans)+1)
	mcpDone := make(chan struct{})
	goRun := a.goRun
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()

	// A chat app that fails at startup stops Thicket (the container
	// restarts and the logs say why).
	for _, ch := range startChans {
		lc, err := a.launch(ch, a.startTok[ch.Name()], true, func(lc *liveChan) {
			if lc.err != nil && ctx.Err() == nil && lc.kept.Load() {
				errc <- fmt.Errorf("%s: %w", lc.ch.Name(), lc.err)
			}
		})
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.live[ch.Name()] = lc
		a.mu.Unlock()
	}
	if a.HTTP != nil {
		goRun("http", func() {
			a.Log.Info("http listening", "addr", a.HTTP.Addr)
			if err := a.HTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("http: %w", err)
			}
		})
	}
	if a.ensureSandbox(ctx) {
		a.keepExecGrants(ctx) // else next start: the sandbox may not be marked exec yet
	}
	goRun("mcp", func() {
		defer close(mcpDone)
		for name, err := range a.MCP.ConnectAll(ctx) {
			if name == sandboxName && a.retrySandbox(ctx) == nil {
				continue // it was still starting
			}
			a.startErr(fmt.Sprintf("The %s integration didn't connect: %s", name, shortErr(err)))
		}
	})
	goRun("pause", func() { a.Engine.RunPauseWatch(ctx) })
	goRun("scheduler", func() { a.Scheduler.Run(ctx) })
	goRun("memory", func() { a.Reviewer.Run(ctx) })
	goRun("initiative", func() { a.Engine.RunInitiative(ctx) })
	goRun("triggers", func() { a.Engine.RunTriggers(ctx) })
	goRun("characters", func() {
		a.Chars.Watch(ctx, 10*time.Second, func(errs []character.LoadError) {
			for _, e := range errs {
				a.Engine.Notify(ctx, "charerr:"+e.Error(), time.Hour, "error", "I couldn't load a character file you changed: "+e.Error())
			}
		})
	})
	goRun("maintenance", func() { a.maintenance(ctx) })
	goRun("backups", func() { a.backups(ctx) })
	goRun("startup-report", func() { a.reportStartup(ctx, mcpDone, interrupted) })
	goRun("first-run-link", func() { a.firstRunLink(ctx) })

	a.Log.Info("thicket started", "version", a.Version, "characters", len(a.Chars.All()))
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
		a.Log.Error("fatal", "err", err)
	}
	cancel()
	a.mu.Lock()
	a.stopping = true // nothing new starts once Run is waiting
	a.mu.Unlock()
	if a.HTTP != nil {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.HTTP.Shutdown(sctx)
		scancel()
	}
	a.wg.Wait()
	a.Engine.WaitTasks()
	return err
}

// reportStartup tells the owner about problems found at startup, once a
// channel is connected.
func (a *App) reportStartup(ctx context.Context, mcpDone <-chan struct{}, interrupted []store.Task) {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		ok := true
		for _, ch := range a.chanList() {
			ok = ok && ch.Connected()
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	a.Engine.ReportInterrupted(ctx, interrupted)
	select { // MCP failures are part of the report
	case <-mcpDone:
	case <-ctx.Done():
		return
	}
	a.mu.Lock()
	errs := append([]string(nil), a.startErrs...)
	a.mu.Unlock()
	if len(errs) > 0 {
		a.Engine.Notify(ctx, "startup:"+strings.Join(errs, "|"), 12*time.Hour, "error",
			"I just woke up and found some problems:\n- "+strings.Join(errs, "\n- "))
	}
}

// firstRunLink logs a sign-in link when nobody owns this Thicket yet, so a
// web-only install can be set up from the container logs.
func (a *App) firstRunLink(ctx context.Context) {
	if has, err := a.DB.HasOwner(ctx); err != nil || has || a.HTTP == nil {
		return
	}
	token, err := a.DB.CreateWebLink(ctx, 0, 24*time.Hour)
	if err != nil {
		a.Log.Error("first-run link", "err", err)
		return
	}
	a.Log.Info("no owner yet: open this link to set up Thicket in your browser (works once, for 24 hours; `thicket web link` makes a new one)",
		"url", a.Cfg.WebBaseURL()+"/login#"+token)
}

func (a *App) maintenance(ctx context.Context) {
	run := func() {
		if n, err := a.DB.SweepMemories(ctx, 30*24*time.Hour); err != nil {
			a.Log.Error("sweep memories", "err", err)
		} else if n > 0 {
			a.Log.Info("expired memories removed", "count", n)
			// In the activity log too: doctor's memory check then knows
			// counts changed since a migration for a good reason.
			_ = a.DB.LogActivity(ctx, store.Activity{Level: "info", Kind: "memory", Summary: fmt.Sprintf("%d expired memories removed for good", n)})
		}
		if _, err := a.DB.PruneMessages(ctx, a.Cfg.MessageRetention); err != nil {
			a.Log.Error("prune messages", "err", err)
		}
		if err := a.DB.PruneWeb(ctx, a.Cfg.MessageRetention); err != nil {
			a.Log.Error("prune web", "err", err)
		}
		if err := a.DB.PruneActivity(ctx, 90*24*time.Hour); err != nil {
			a.Log.Error("prune activity", "err", err)
		}
		if ok, _ := a.DB.ClaimNotice(ctx, "snapshot", 23*time.Hour); ok {
			if path, err := backup.Snapshot(ctx, a.DB, filepath.Join(a.Cfg.DataDir, "backups"), 7); err != nil {
				a.Engine.Notify(ctx, "snapshot-fail", 24*time.Hour, "error", "Last night's database backup failed: "+shortErr(err))
			} else {
				a.Log.Info("daily snapshot written", "path", path)
			}
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func (a *App) Close() {
	backup.ClearHeartbeat(a.Cfg.DataDir)
	if a.MCP != nil {
		a.MCP.Close()
	}
	if a.DB != nil {
		a.DB.Close()
	}
}

// Webhook starts the webhook job a token belongs to. Unknown tokens get
// the same answer as a missing route, and nothing about the job leaks.
func (a *App) Webhook(ctx context.Context, token string, body []byte, header http.Header) (int, string) {
	if !strings.HasPrefix(token, "thk_") || len(token) > 100 {
		return http.StatusNotFound, "not found"
	}
	j, err := a.DB.JobByHook(ctx, agent.HookHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound, "not found"
	}
	if err != nil {
		a.Log.Error("webhook lookup", "err", err)
		return http.StatusInternalServerError, "internal error"
	}
	switch err := a.Engine.CheckWebhook(ctx, *j, body, header); {
	case errors.Is(err, agent.ErrDuplicate):
		return http.StatusOK, "already received"
	case err != nil:
		return http.StatusUnauthorized, err.Error()
	}
	// A filter looks at the body as sent, before headers are added. Only
	// "no filter" lets a call through unfiltered; a failed lookup doesn't.
	tr, err := a.DB.TriggerFor(ctx, j.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		a.Log.Error("webhook trigger lookup", "job", j.ID, "err", err)
		a.Engine.ReleaseDelivery(ctx, *j, header) // let the sender's retry through
		return http.StatusServiceUnavailable, "try again shortly"
	case tr.Source == store.TriggerWebhook && !agent.Matches(tr.Match, string(body)):
		return http.StatusOK, "ignored: the body doesn't match this job's filter"
	}
	payload := string(body)
	if !utf8.ValidString(payload) {
		payload = fmt.Sprintf("(%d bytes of binary data, not shown)", len(body))
	}
	var head []string
	if ev := agent.WebhookEvent(header); ev != "" {
		head = append(head, "Event: "+ev)
	}
	if ct := strings.TrimSpace(header.Get("Content-Type")); ct != "" && payload != "" {
		head = append(head, "Content-Type: "+ct)
	}
	if len(head) > 0 {
		payload = strings.Join(head, "\n") + "\n\n" + payload
	}
	j.Payload = payload
	fireErr := a.Scheduler.Fire(*j)
	if fireErr != nil {
		// Not run: let the sender's retry of this delivery through.
		a.Engine.ReleaseDelivery(ctx, *j, header)
	}
	switch err := fireErr; {
	case err == nil:
		return http.StatusAccepted, "started"
	case errors.Is(err, scheduler.ErrPaused):
		return http.StatusConflict, err.Error()
	case errors.Is(err, scheduler.ErrHeld):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, scheduler.ErrBusy), errors.Is(err, scheduler.ErrTooSoon), errors.Is(err, scheduler.ErrTooMany):
		a.Engine.WebhookRefused(ctx, *j, err)
		return http.StatusTooManyRequests, err.Error()
	default:
		a.Log.Error("webhook fire", "job", j.ID, "err", err)
		return http.StatusInternalServerError, "internal error"
	}
}
