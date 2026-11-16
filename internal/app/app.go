// Package app wires Thicket together. cmd/thicket and the smoke test both
// build the whole system through here.
package app

import (
	"context"
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
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/budget"
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
)

type Options struct {
	// Model overrides the OpenRouter client (tests).
	Model llm.Client
	// KeyCheck overrides OpenRouter key validation (tests).
	KeyCheck func(ctx context.Context, key string) error
	// Channels builds the chat adapters with Thicket's (redacting) logger.
	Channels func(log *slog.Logger) []channel.Channel
	LogOut   io.Writer
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
	Engine    *agent.Engine
	Reviewer  *memory.Reviewer
	Scheduler *scheduler.Scheduler
	API       *api.API
	Dashboard *dashboard.Dashboard
	HTTP      *http.Server
	channels  []channel.Channel

	mu        sync.Mutex
	startErrs []string
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
	a := &App{Cfg: cfg, Version: version, Log: log}
	if opts.Channels != nil {
		a.channels = opts.Channels(log)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "thicket.db"))
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	a.DB = db

	key, err := secrets.LoadKey(cfg.SecretKey, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	sec, err := secrets.New(db, key, red)
	if err != nil {
		return nil, err
	}
	sec.SetEnvFallback(secrets.OpenRouterKey, cfg.OpenRouterKey)
	if err := sec.WarmRedactor(ctx); err != nil {
		a.startErr("Some stored secrets couldn't be decrypted (" + err.Error() + "). Did THICKET_SECRET_KEY or secret.key change?")
		log.Error("secrets", "err", err)
	}
	a.Secrets = sec
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

	sec.SetEnvFallback(secrets.ModelAPIKey, cfg.ModelAPIKey)
	// The address and key are read per call: onboarding can switch to a
	// local server, and keys added in chat apply at once.
	or := llm.NewOpenRouter(cfg.ModelURL, func(ctx context.Context) (string, error) {
		if llm.IsOpenRouter(a.Settings.String(ctx, "model_url")) {
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
	a.Guard = &budget.Guard{Next: router, DB: db, Settings: a.Settings, Log: log,
		OnAlert: func(ctx context.Context, al budget.Alert) {
			a.Engine.Notify(ctx, al.Key, 31*24*time.Hour, al.Level, al.Message)
		}}
	a.Engine.LLM = a.Guard
	a.Engine.Budget = a.Guard

	a.MCP = mcp.NewRegistry(db, sec, log)
	a.Engine.MCP = a.MCP
	for _, ch := range a.channels {
		a.Engine.AddChannel(ch)
	}

	a.Reviewer = &memory.Reviewer{DB: db, LLM: a.Guard, Settings: a.Settings, Chars: a.Chars, Log: log, Idle: cfg.IdleAfter,
		OnFailure: func(ctx context.Context, userID int64, err error) {
			a.Engine.Notify(ctx, "memory-review", 6*time.Hour, "error",
				"My memory review keeps failing ("+shortErr(err)+"). Nothing is lost, but I'm not learning from recent chats until it's fixed.")
		}}
	a.Scheduler = scheduler.New(db, a.Engine, log)
	// The local API is an internal channel: replies to its messages come
	// back through it. It isn't listed in /health like the chat apps.
	a.API = api.New(a.Engine, db, a.Scheduler, a.Chars, log)
	a.Engine.AddChannel(a.API)
	a.Dashboard = &dashboard.Dashboard{DB: db, Chars: a.Chars, Settings: a.Settings, Version: version, Log: log,
		Secure: strings.HasPrefix(cfg.PublicURL, "https://"),
		Health: func(ctx context.Context) map[string]dashboard.Check {
			out := map[string]dashboard.Check{}
			for k, c := range a.health(ctx) {
				out[k] = dashboard.Check{OK: c.OK, Detail: c.Detail}
			}
			return out
		}}

	if !opts.NoHTTP {
		a.HTTP = server.New(cfg.HTTPAddr, version, cfg.CharactersDir, a.health, a.Webhook, agent.MaxWebhookPayload, a.API.Handler(), a.Dashboard.Handler())
	}
	return a, nil
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
	for _, ch := range a.channels {
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
	return out
}

// Run starts everything and blocks until ctx is cancelled.
func (a *App) Run(parent context.Context) error {
	// Workers get a child context so every exit path stops them, and Run
	// waits for them before returning (Close must not race live workers).
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	a.Engine.SetLifetime(ctx) // background tasks end with Thicket
	var wg sync.WaitGroup
	errc := make(chan error, len(a.channels)+1)
	mcpDone := make(chan struct{})
	goRun := func(name string, f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
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

	for _, ch := range a.channels {
		ch := ch
		goRun("channel "+ch.Name(), func() {
			if err := ch.Run(ctx, a.Engine.HandleIncoming); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", ch.Name(), err)
			}
		})
	}
	if a.HTTP != nil {
		goRun("http", func() {
			a.Log.Info("http listening", "addr", a.HTTP.Addr)
			if err := a.HTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("http: %w", err)
			}
		})
	}
	goRun("mcp", func() {
		defer close(mcpDone)
		for name, err := range a.MCP.ConnectAll(ctx) {
			a.startErr(fmt.Sprintf("The %s integration didn't connect: %s", name, shortErr(err)))
		}
	})
	goRun("scheduler", func() { a.Scheduler.Run(ctx) })
	goRun("memory", func() { a.Reviewer.Run(ctx) })
	goRun("initiative", func() { a.Engine.RunInitiative(ctx) })
	goRun("characters", func() {
		a.Chars.Watch(ctx, 10*time.Second, func(errs []character.LoadError) {
			for _, e := range errs {
				a.Engine.Notify(ctx, "charerr:"+e.Error(), time.Hour, "error", "I couldn't load a character file you changed: "+e.Error())
			}
		})
	})
	goRun("maintenance", func() { a.maintenance(ctx) })
	goRun("startup-report", func() { a.reportStartup(ctx, mcpDone) })

	a.Log.Info("thicket started", "version", a.Version, "characters", len(a.Chars.All()))
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
		a.Log.Error("fatal", "err", err)
	}
	cancel()
	if a.HTTP != nil {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.HTTP.Shutdown(sctx)
		scancel()
	}
	wg.Wait()
	a.Engine.WaitTasks()
	return err
}

// reportStartup tells the owner about problems found at startup, once a
// channel is connected.
func (a *App) reportStartup(ctx context.Context, mcpDone <-chan struct{}) {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		ok := len(a.channels) == 0
		for _, ch := range a.channels {
			ok = ok || ch.Connected()
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

func (a *App) maintenance(ctx context.Context) {
	run := func() {
		if n, err := a.DB.SweepMemories(ctx, 30*24*time.Hour); err != nil {
			a.Log.Error("sweep memories", "err", err)
		} else if n > 0 {
			a.Log.Info("expired memories removed", "count", n)
		}
		if _, err := a.DB.PruneMessages(ctx, a.Cfg.MessageRetention); err != nil {
			a.Log.Error("prune messages", "err", err)
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
	switch err := a.Scheduler.Fire(*j); {
	case err == nil:
		return http.StatusAccepted, "started"
	case errors.Is(err, scheduler.ErrPaused):
		return http.StatusConflict, err.Error()
	case errors.Is(err, scheduler.ErrBusy), errors.Is(err, scheduler.ErrTooSoon), errors.Is(err, scheduler.ErrTooMany):
		a.Engine.WebhookRefused(ctx, *j, err)
		return http.StatusTooManyRequests, err.Error()
	default:
		a.Log.Error("webhook fire", "job", j.ID, "err", err)
		return http.StatusInternalServerError, "internal error"
	}
}
