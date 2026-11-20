package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/plugin"
	"github.com/theshield2594/thicket/internal/store"
)

// Plugins are optional abilities in their own containers (internal/plugin,
// docs/plugins.md); the sandbox computer is the first. Each is an ordinary
// MCP integration named after the plugin, set up from its
// THICKET_<NAME>_URL (or, when that isn't set, its default address, if
// it answers): no sprig can use it until its owner grants
// mcp:<name>/*, and its tools that change things ask for approval like
// any others. A plugin with no address, or a default one that doesn't
// answer, is simply absent.

const sandboxName = "sandbox"

// plugins returns the known plugins (tests replace them).
func (a *App) plugins() *plugin.Registry {
	if a.Plugins != nil {
		return a.Plugins
	}
	return plugin.Builtin()
}

// pluginEndpoints says where each known plugin is configured to be.
func (a *App) pluginEndpoints() []plugin.Endpoint {
	var out []plugin.Endpoint
	for _, s := range a.plugins().All() {
		if ep := s.Resolve(a.Cfg.LookupPluginEnv, a.Cfg.DataDir); ep.URL != "" {
			out = append(out, ep)
		}
	}
	return out
}

// ensurePlugins sets up the plugins whose address was set: it shares a
// token with each (through a file both containers mount) and keeps its
// integration's settings current. Plugins only at their default address
// are tried in the background (watchDefaultPlugins). It reports whether
// every set-up plugin's settings are known to be current.
func (a *App) ensurePlugins(ctx context.Context) bool {
	ok := true
	for _, ep := range a.pluginEndpoints() {
		if !ep.Explicit {
			continue
		}
		if err := a.setUpPlugin(ctx, ep); err != nil {
			a.startErr(fmt.Sprintf("The %s plugin isn't set up: %s", ep.Name, shortErr(err)))
			ok = false
		}
	}
	return ok
}

// ensureSandbox is ensurePlugins, kept for the sandbox's callers.
func (a *App) ensureSandbox(ctx context.Context) bool { return a.ensurePlugins(ctx) }

func (a *App) setUpPlugin(ctx context.Context, ep plugin.Endpoint) error {
	tok := ""
	if b, err := os.ReadFile(ep.TokenFile); err == nil {
		tok = strings.TrimSpace(string(b))
	}
	if len(tok) < 32 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		tok = base64.RawURLEncoding.EncodeToString(b)
		if err := os.MkdirAll(filepath.Dir(ep.TokenFile), 0o755); err != nil {
			return fmt.Errorf("token folder: %w", err)
		}
		// Readable by the plugin's own user: the file only unlocks the
		// plugin itself.
		if err := os.WriteFile(ep.TokenFile, []byte(tok+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing the token: %w", err)
		}
	}
	if cur, _ := a.Secrets.Get(ctx, ep.Secret()); cur != tok {
		if err := a.Secrets.Put(ctx, ep.Secret(), tok); err != nil {
			return err
		}
	}
	want := store.MCPServer{Name: ep.Name, Transport: "http", Target: ep.MCPURL(),
		Headers: map[string]string{"Authorization": "Bearer {{secret:" + ep.Secret() + "}}"}, Enabled: true, Exec: ep.Exec}
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name == ep.Name && s.Transport == want.Transport && s.Target == want.Target && maps.Equal(s.Headers, want.Headers) && s.Enabled && s.Exec == want.Exec {
			return nil
		}
	}
	return a.DB.PutMCPServer(ctx, want)
}

// isPlugin reports whether an integration is a configured plugin.
func (a *App) isPlugin(name string) bool {
	for _, ep := range a.pluginEndpoints() {
		if ep.Name == name {
			return true
		}
	}
	return false
}

// retryPlugin waits for a plugin that is still starting.
func (a *App) retryPlugin(ctx context.Context, name string) error {
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name != name {
			continue
		}
		for i := 0; i < 6; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pluginRetry):
			}
			if _, err = a.MCP.Connect(ctx, s); err == nil {
				return nil
			}
		}
		return err
	}
	return fmt.Errorf("no %s integration", name)
}

// retrySandbox waits for a sandbox that is still starting.
func (a *App) retrySandbox(ctx context.Context) error { return a.retryPlugin(ctx, sandboxName) }

// pluginRetry is how long to wait between tries of a plugin that is
// still starting (tests shorten it).
var pluginRetry = 5 * time.Second

// watchDefaultPlugins tries the plugins that are only at their default
// address for a minute after start (their containers may still be
// starting): one that answers is set up and connected; one that never
// does stays absent, with no error. One set up on an earlier start that
// isn't running now is turned off, so it isn't reported as broken.
func (a *App) watchDefaultPlugins(ctx context.Context) {
	for _, ep := range a.pluginEndpoints() {
		if ep.Explicit {
			continue
		}
		answered := false
		for i := 0; i < 12 && !answered; i++ {
			if i > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pluginRetry):
				}
			}
			answered = plugin.Answers(ctx, ep.URL)
		}
		if !answered {
			a.absentPlugin(ctx, ep)
			continue
		}
		if err := a.setUpPlugin(ctx, ep); err != nil {
			a.Log.Warn("plugin not set up", "plugin", ep.Name, "err", shortErr(err))
			continue
		}
		if err := a.retryPluginNow(ctx, ep.Name); err != nil {
			a.Log.Warn("plugin answered but didn't connect", "plugin", ep.Name, "err", shortErr(err))
			continue
		}
		a.Log.Info("plugin connected", "plugin", ep.Name, "url", ep.URL)
	}
}

// retryPluginNow connects a plugin, waiting a little if it is starting.
func (a *App) retryPluginNow(ctx context.Context, name string) error {
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name == name {
			if _, err := a.MCP.Connect(ctx, s); err == nil {
				return nil
			}
			return a.retryPlugin(ctx, name)
		}
	}
	return fmt.Errorf("no %s integration", name)
}

// absentPlugin turns off a default-address plugin's integration left from
// an earlier start, if it is still the one Thicket set up. Grants stay,
// so starting the plugin again brings it back.
func (a *App) absentPlugin(ctx context.Context, ep plugin.Endpoint) {
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return
	}
	for _, s := range servers {
		if s.Name == ep.Name && s.Enabled && s.Target == ep.MCPURL() {
			s.Enabled = false
			_ = a.DB.PutMCPServer(ctx, s)
		}
	}
}

// execKeptKey marks that sprigs given an exec-capable integration before
// exec became a separate opt-in have kept it.
const execKeptKey = "sandboxing.exec_kept"

// keepExecGrants runs once, on the first start after the upgrade that
// made exec opt-in: a sprig that already had an exec-capable integration
// (the sandbox) keeps using it, with exec turned on and a trail entry
// saying so. Sprigs given one later need exec turned on explicitly.
func (a *App) keepExecGrants(ctx context.Context) {
	if _, done, err := a.DB.Setting(ctx, execKeptKey); err != nil || done {
		return
	}
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return
	}
	for _, c := range a.Chars.All() {
		if c.IsKid() {
			continue
		}
		for _, s := range servers {
			if !s.Exec || !hasServerScope(c.Tools, s.Name) {
				continue
			}
			if err := a.DB.SetExecAllowed(ctx, c.ID, true, 0); err != nil {
				a.Log.Error("keep exec", "sprig", c.ID, "err", err)
				return // try again next start
			}
			_ = a.DB.AddTrail(ctx, store.TrailEntry{Character: c.ID, Kind: store.TrailAccess,
				Summary: fmt.Sprintf("%s's exec: on (kept: it had the %s integration before exec became a separate opt-in)", c.DisplayName(), s.Name)})
			break
		}
	}
	_ = a.DB.SetSetting(ctx, execKeptKey, "1")
}

// hasServerScope reports whether scopes grant any tool of server.
func hasServerScope(scopes []string, server string) bool {
	for _, s := range scopes {
		if s == "mcp:"+server || strings.HasPrefix(s, "mcp:"+server+"/") {
			return true
		}
	}
	return false
}
