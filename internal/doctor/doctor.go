// Package doctor checks a Thicket installation end to end: configuration,
// database, stored secrets, the model server, chat app tokens, each MCP
// server, and the running server's live state. `thicket doctor` prints it.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/plugin"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// Check is one finding. Optional ones are reported but don't make the
// doctor fail.
type Check struct {
	Name     string
	OK       bool
	Optional bool
	Detail   string
}

type Doctor struct {
	Cfg  *config.Config
	HTTP *http.Client
	Log  *slog.Logger
	// API bases, replaced in tests.
	DiscordAPI  string
	TelegramAPI string
	// ServerURL is the running server's base URL; empty means
	// http://127.0.0.1<THICKET_HTTP_ADDR>.
	ServerURL string
	// Plugins are the known optional containers (nil: plugin.Builtin).
	Plugins *plugin.Registry
	// DataOnly checks only the data (config, database, memory, secrets),
	// with no network calls.
	DataOnly bool
}

func (d *Doctor) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Run performs every check. It opens the database (applying migrations,
// as startup does) and connects to each enabled MCP server briefly.
func (d *Doctor) Run(ctx context.Context) []Check {
	var out []Check
	add := func(c Check) { out = append(out, c) }

	fatal := []string{}
	for _, p := range d.Cfg.Check() {
		if p.Fatal {
			fatal = append(fatal, p.What)
		}
	}
	add(Check{Name: "config", OK: len(fatal) == 0, Detail: orDefault(strings.Join(fatal, "; "), "required settings present")})

	db, err := store.Open(ctx, filepath.Join(d.Cfg.DataDir, "thicket.db"))
	if err != nil {
		add(Check{Name: "database", Detail: err.Error()})
		return out // everything else needs it
	}
	defer db.Close()
	ver, _ := db.SchemaVersion(ctx)
	if err := db.QuickCheck(ctx); err != nil {
		add(Check{Name: "database", Detail: fmt.Sprintf("schema v%d, integrity check failed: %v", ver, err)})
	} else {
		add(Check{Name: "database", OK: true, Detail: fmt.Sprintf("schema v%d, integrity ok", ver)})
	}
	add(d.Memory(ctx, db))

	red := secrets.NewRedactor()
	red.Add(d.Cfg.ModelAPIKey)
	red.Add(d.Cfg.DiscordToken)
	red.Add(d.Cfg.TelegramToken)
	red.Add(d.Cfg.MatrixToken)
	var sec *secrets.Store
	if key, _, err := secrets.ResolveKey(d.Cfg.SecretKey, d.Cfg.SecretKeyFile, d.Cfg.DataDir); err != nil {
		add(Check{Name: "secrets", Detail: err.Error()})
	} else if sec, err = secrets.New(db, key, red); err != nil {
		add(Check{Name: "secrets", Detail: err.Error()})
	} else if err := sec.WarmRedactor(ctx); err != nil {
		add(Check{Name: "secrets", Detail: "some stored secrets can't be decrypted (did THICKET_SECRET_KEY or secret.key change?): " + err.Error()})
	} else {
		add(Check{Name: "secrets", OK: true, Detail: "all stored secrets decrypt"})
	}
	if sec != nil {
		sec.SetEnvFallback(secrets.OpenRouterKey, d.Cfg.OpenRouterKey)
		sec.SetEnvFallback(secrets.ModelAPIKey, d.Cfg.ModelAPIKey)
		// Chat tokens may be stored rather than in the environment.
		cfg := *d.Cfg
		if v, err := sec.Get(ctx, secrets.DiscordToken); err == nil && cfg.DiscordToken == "" {
			cfg.DiscordToken = v
		}
		if v, err := sec.Get(ctx, secrets.TelegramToken); err == nil && cfg.TelegramToken == "" {
			cfg.TelegramToken = v
		}
		if v, err := sec.Get(ctx, secrets.MatrixToken); err == nil && cfg.MatrixToken == "" {
			cfg.MatrixToken = v
		}
		d.Cfg = &cfg
	}

	if d.DataOnly {
		return out
	}
	add(d.model(ctx, settings.New(db, d.Cfg), sec, red))
	if d.Cfg.DiscordToken != "" {
		add(d.discord(ctx, red))
	}
	if d.Cfg.TelegramToken != "" {
		add(d.telegram(ctx, red))
	}
	if d.Cfg.MatrixOn() && d.Cfg.MatrixToken != "" {
		add(d.matrix(ctx, red))
	}
	if sec != nil {
		out = append(out, d.mcpServers(ctx, db, sec)...)
	}
	out = append(out, d.plugins(ctx, sec)...)
	out = append(out, d.server(ctx)...)
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// get does a GET and returns the body of a 2xx response.
func (d *Doctor) get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return body, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (d *Doctor) model(ctx context.Context, st *settings.Settings, sec *secrets.Store, red *secrets.Redactor) Check {
	base := strings.TrimRight(st.String(ctx, "model_url"), "/")
	c := Check{Name: "model server"}
	key := ""
	if sec != nil {
		name := secrets.ModelAPIKey
		if llm.IsOpenRouter(base) {
			name = secrets.OpenRouterKey
		}
		key, _ = sec.Get(ctx, name)
	}
	var auth map[string]string
	if key != "" && !llm.IsOpenRouter(base) {
		auth = map[string]string{"Authorization": "Bearer " + key} // a provider, or a protected local server
		if p, ok := llm.ProviderFor(base); ok && p.Headers != nil {
			for k, v := range p.Headers(key) {
				auth[k] = v
			}
		}
	}
	if p, ok := llm.ProviderFor(base); ok {
		if key == "" {
			c.Detail = p.Name + " is set, but no key is stored yet: DM Juni the key with the provider's name"
			return c
		}
		if _, err := d.get(ctx, base+"/models", auth); err != nil {
			c.Detail = p.Name + " refused the stored key or isn't reachable (" + red.Redact(err.Error()) + ")"
			return c
		}
		c.OK, c.Detail = true, p.Name+" reachable, key accepted"
		return c
	}
	if _, err := d.get(ctx, base+"/models", auth); err != nil {
		c.Detail = fmt.Sprintf("%s isn't reachable: %s", base, red.Redact(err.Error()))
		return c
	}
	if !llm.IsOpenRouter(base) {
		c.OK, c.Detail = true, base+" reachable"
		return c
	}
	if key == "" {
		c.Detail = "OpenRouter is reachable, but no key is stored yet: DM Juni to add one"
		return c
	}
	if _, err := d.get(ctx, base+"/key", map[string]string{"Authorization": "Bearer " + key}); err != nil {
		c.Detail = "OpenRouter is reachable, but the stored key was refused (" + red.Redact(err.Error()) + ")"
		return c
	}
	c.OK, c.Detail = true, "OpenRouter reachable, key accepted"
	return c
}

func (d *Doctor) discord(ctx context.Context, red *secrets.Redactor) Check {
	base := orDefault(d.DiscordAPI, "https://discord.com/api/v10")
	body, err := d.get(ctx, base+"/users/@me", map[string]string{"Authorization": "Bot " + d.Cfg.DiscordToken})
	if err != nil {
		return Check{Name: "discord token", Detail: "Discord refused the token or isn't reachable: " + red.Redact(err.Error())}
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(body, &me)
	return Check{Name: "discord token", OK: true, Detail: "valid, bot " + orDefault(me.Username, "(unnamed)")}
}

func (d *Doctor) telegram(ctx context.Context, red *secrets.Redactor) Check {
	base := orDefault(d.TelegramAPI, "https://api.telegram.org")
	body, err := d.get(ctx, base+"/bot"+d.Cfg.TelegramToken+"/getMe", nil)
	if err != nil {
		// The token is in the URL: keep it out of the message.
		return Check{Name: "telegram token", Detail: "Telegram refused the token or isn't reachable: " + red.Redact(err.Error())}
	}
	var me struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &me) != nil || !me.OK {
		return Check{Name: "telegram token", Detail: "unexpected answer from Telegram"}
	}
	return Check{Name: "telegram token", OK: true, Detail: "valid, bot @" + me.Result.Username}
}

func (d *Doctor) matrix(ctx context.Context, red *secrets.Redactor) Check {
	body, err := d.get(ctx, d.Cfg.MatrixHomeserver+"/_matrix/client/v3/account/whoami", map[string]string{"Authorization": "Bearer " + d.Cfg.MatrixToken})
	if err != nil {
		return Check{Name: "matrix token", Detail: "the homeserver refused the access token or isn't reachable: " + red.Redact(err.Error())}
	}
	var me struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(body, &me) != nil || me.UserID == "" {
		return Check{Name: "matrix token", Detail: "unexpected answer from " + d.Cfg.MatrixHomeserver + " (is it the client-server API address?)"}
	}
	return Check{Name: "matrix token", OK: true, Detail: "valid, account " + me.UserID}
}

func (d *Doctor) mcpServers(ctx context.Context, db *store.Store, sec *secrets.Store) []Check {
	servers, err := db.MCPServers(ctx)
	if err != nil {
		return []Check{{Name: "mcp", Detail: err.Error()}}
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	reg := mcp.NewRegistry(db, sec, log)
	defer reg.Close()
	var out []Check
	for _, s := range servers {
		if !s.Enabled {
			out = append(out, Check{Name: "mcp." + s.Name, OK: true, Optional: true, Detail: "disabled"})
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		tools, err := reg.Connect(cctx, s)
		cancel()
		if err != nil {
			out = append(out, Check{Name: "mcp." + s.Name, Optional: true, Detail: "not responding: " + sec.Redact(err.Error())})
			continue
		}
		out = append(out, Check{Name: "mcp." + s.Name, OK: true, Optional: true, Detail: fmt.Sprintf("responding, %d tools", len(tools))})
	}
	return out
}

// plugins checks each known plugin: whether it answers, accepts its
// token, and which version it is, then its own check if it has one. A
// plugin that is off, or not running at its default address, is only
// reported; one whose address was set and fails is a problem.
func (d *Doctor) plugins(ctx context.Context, sec *secrets.Store) []Check {
	reg := d.Plugins
	if reg == nil {
		reg = plugin.Builtin()
	}
	var out []Check
	for _, s := range reg.All() {
		ep := s.Resolve(d.Cfg.LookupPluginEnv, d.Cfg.DataDir)
		c := Check{Name: "plugin." + s.Name, Optional: true}
		start := "set " + s.URLEnv()
		if s.Profile != "" {
			start = "start it with `docker compose --profile " + s.Profile + " up -d`"
		}
		switch {
		case ep.URL == "":
			c.OK, c.Detail = true, "off ("+start+" to use it)"
		case !plugin.Answers(ctx, ep.URL) && !ep.Explicit:
			c.OK, c.Detail = true, "not running ("+start+" to use it)"
		default:
			c = d.pluginCheck(ctx, ep, sec, c)
		}
		out = append(out, c)
	}
	return out
}

func (d *Doctor) pluginCheck(ctx context.Context, ep plugin.Endpoint, sec *secrets.Store, c Check) Check {
	tok := ""
	if sec != nil {
		tok, _ = sec.Get(ctx, ep.Secret())
	}
	if tok == "" {
		if b, err := os.ReadFile(ep.TokenFile); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" {
		c.Detail = "no token shared with it yet (Thicket makes one when it starts with " + ep.URLEnv() + " set)"
		return c
	}
	redact := func(s string) string {
		s = strings.ReplaceAll(s, tok, "[token]")
		if sec != nil {
			s = sec.Redact(s)
		}
		return s
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cl, err := mcp.DialHTTP(cctx, ep.MCPURL(), map[string]string{"Authorization": "Bearer " + tok})
	switch {
	case err != nil && (strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403")):
		c.Detail = "answers at " + ep.URL + " but refused the token (is " + ep.TokenFileEnv() + " the same file in both containers?)"
		return c
	case err != nil:
		c.Detail = "not responding at " + ep.URL + ": " + redact(err.Error())
		return c
	}
	defer cl.Close()
	tools, err := cl.ListTools(cctx)
	if err != nil {
		c.Detail = "connected but couldn't list its tools: " + redact(err.Error())
		return c
	}
	_, ver := cl.ServerInfo()
	if ver == "" {
		ver = "unknown version"
	} else {
		ver = "version " + ver
	}
	if ep.Check != nil {
		if err := ep.Check(cctx, ep.URL, tok); err != nil {
			c.Detail = fmt.Sprintf("%s, token accepted, %s, but: %s", ep.URL, ver, redact(err.Error()))
			return c
		}
	}
	c.OK, c.Detail = true, fmt.Sprintf("%s, token accepted, %s, %d tools", ep.URL, ver, len(tools))
	return c
}

// server asks the running Thicket for its live state (chat connections,
// scheduler). If it isn't running here, that's reported, not failed.
func (d *Doctor) server(ctx context.Context) []Check {
	base := d.ServerURL
	if base == "" {
		addr := d.Cfg.HTTPAddr
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		base = "http://" + addr
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return []Check{{Name: "server", Optional: true, Detail: err.Error()}}
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return []Check{{Name: "server", Optional: true, Detail: "not running at " + base + ", so live connections weren't checked"}}
	}
	defer resp.Body.Close()
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Checks  map[string]struct {
			OK       bool   `json:"ok"`
			Detail   string `json:"detail"`
			Optional bool   `json:"optional"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h); err != nil {
		return []Check{{Name: "server", Detail: "unreadable /health answer from " + base}}
	}
	names := make([]string, 0, len(h.Checks))
	for n := range h.Checks {
		names = append(names, n)
	}
	sort.Strings(names)
	// The model key was checked above: don't fail twice for it.
	var failing []string
	for _, n := range names {
		if c := h.Checks[n]; !c.OK && !c.Optional && n != "model_key" {
			failing = append(failing, n)
		}
	}
	detail := fmt.Sprintf("running, version %s", h.Version)
	if len(failing) > 0 {
		detail += ", failing: " + strings.Join(failing, ", ")
	}
	out := []Check{{Name: "server", OK: len(failing) == 0, Detail: detail}}
	for _, n := range names {
		c := h.Checks[n]
		// Live chat connections and the scheduler are what only the
		// running server knows; the rest was checked above.
		if !strings.HasPrefix(n, "channel.") && n != "scheduler" {
			continue
		}
		detail := c.Detail
		if strings.HasPrefix(n, "channel.") {
			detail = orDefault(detail, map[bool]string{true: "connected", false: "not connected"}[c.OK])
		}
		out = append(out, Check{Name: "live " + strings.TrimPrefix(n, "channel."), OK: c.OK, Optional: c.Optional, Detail: detail})
	}
	return out
}

// Memory checks that memory survived: counts per person and sprig, no
// memories of people who no longer exist, every memory a person set still
// locked, and, right after a migration, nothing lost compared with the
// snapshot taken before it. A drop fails the check and names the layer.
func (d *Doctor) Memory(ctx context.Context, db *store.Store) Check {
	c := Check{Name: "memory"}
	now, err := db.MemoryCensus(ctx)
	if err != nil {
		c.Detail = "couldn't count memories: " + err.Error()
		return c
	}
	reg := character.NewRegistry(d.Cfg.CharactersDir, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = reg.Load()
	name := func(id string) string {
		if ch, ok := reg.Get(id); ok {
			return ch.DisplayName()
		}
		return id
	}
	var problems, notes []string
	if now.OrphanUsers > 0 {
		problems = append(problems, fmt.Sprintf("%d memories belong to people who no longer exist", now.OrphanUsers))
	}
	if now.Unlocked > 0 {
		problems = append(problems, fmt.Sprintf("%d memories a person set are no longer locked", now.Unlocked))
	}
	sprigs := now.SprigsWithMemories()
	ids := make([]string, 0, len(sprigs))
	for id := range sprigs {
		if _, ok := reg.Get(id); !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		// A sprig file removed by hand leaves its memories behind: worth
		// knowing, not a failure (the migration comparison below catches
		// a migration that moved them).
		notes = append(notes, fmt.Sprintf("%d memories belong to sprig %q, which has no character file", sprigs[id], id))
	}
	if n, err := d.compareSnapshot(ctx, db, now, name); err != nil {
		notes = append(notes, "couldn't compare with the pre-migration snapshot: "+err.Error())
	} else {
		problems = append(problems, n.lost...)
		if n.note != "" {
			notes = append(notes, n.note)
		}
	}
	c.OK = len(problems) == 0
	parts := append(problems, now.Summary(name))
	c.Detail = strings.Join(append(parts, notes...), "; ")
	return c
}

type snapshotResult struct {
	lost []string
	note string
}

// compareSnapshot compares memory with the snapshot taken before the
// latest migration, while Thicket hasn't been used since (people forget
// memories in ordinary use, which isn't a loss).
func (d *Doctor) compareSnapshot(ctx context.Context, db *store.Store, now *store.MemoryCensus, name func(string) string) (snapshotResult, error) {
	var res snapshotResult
	applied, err := db.AppliedMigrations(ctx)
	if err != nil || len(applied) == 0 {
		return res, err
	}
	last := applied[len(applied)-1]
	snaps, _ := filepath.Glob(db.Path() + ".pre-v" + strconv.Itoa(last.Version) + ".*.bak")
	if len(snaps) == 0 {
		return res, nil
	}
	sort.Strings(snaps) // the stamp sorts by time
	snap := snaps[len(snaps)-1]
	used, err := db.UsedSince(ctx, last.AppliedAt.Unix())
	if err != nil {
		return res, err
	}
	if used {
		res.note = fmt.Sprintf("Thicket has run since the migration to v%d, so memory is compared with its snapshot only straight after a migration", last.Version)
		return res, nil
	}
	old, err := store.OpenSnapshot(ctx, snap)
	if err != nil {
		return res, err
	}
	defer old.Close()
	before, err := old.MemoryCensus(ctx)
	if err != nil {
		return res, err
	}
	for _, l := range store.MemoryLoss(before, now, name) {
		res.lost = append(res.lost, fmt.Sprintf("lost in the migration to v%d: %s", last.Version, l))
	}
	if len(res.lost) == 0 {
		res.note = fmt.Sprintf("all %d memories from before the migration to v%d are there", before.Total(), last.Version)
	}
	return res, nil
}
