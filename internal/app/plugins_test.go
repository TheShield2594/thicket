package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/doctor"
	"github.com/theshield2594/thicket/internal/plugin"
)

// stubPlugin is a plugin container: an MCP server behind the token in
// tokenFile, saying it is version 1.2.3.
func stubPlugin(t *testing.T, tokenFile string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, "ok")
			return
		}
		tok, _ := os.ReadFile(tokenFile)
		if want := strings.TrimSpace(string(tok)); want == "" || r.Header.Get("Authorization") != "Bearer "+want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"stub","version":"1.2.3"}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"wave","description":"wave hello","inputSchema":{"type":"object"}}]}}`, *req.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"hi"}]}}`, *req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doctorCheck(t *testing.T, a *App, reg *plugin.Registry, name string) doctor.Check {
	t.Helper()
	cfg := *a.Cfg
	cfg.HTTPAddr = "127.0.0.1:1" // no live server to ask
	for _, c := range (&doctor.Doctor{Cfg: &cfg, Plugins: reg}).Run(context.Background()) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("doctor has no %s check", name)
	return doctor.Check{}
}

// A plugin at its default address that answers is connected as a
// built-in integration and shows in `thicket doctor`.
func TestStubPluginRegistersAndShowsInDoctor(t *testing.T) {
	old := pluginRetry
	pluginRetry = 10 * time.Millisecond
	defer func() { pluginRetry = old }()
	a, _, _, ctx := setup(t)
	tokenFile := filepath.Join(t.TempDir(), "stub", "token")
	srv := stubPlugin(t, tokenFile)
	reg := plugin.MustNew(plugin.Sandbox, plugin.Spec{Name: "stub", Profile: "stub", DefaultURL: srv.URL})
	a.Plugins = reg
	a.Cfg.SetPluginEnv("THICKET_STUB_TOKEN_FILE", tokenFile)

	a.watchDefaultPlugins(ctx)
	servers, _ := a.DB.MCPServers(ctx)
	if len(servers) != 1 || servers[0].Name != "stub" || servers[0].Target != srv.URL+"/mcp" || !servers[0].Enabled || servers[0].Exec {
		t.Fatalf("stored: %+v", servers)
	}
	if st := a.MCP.Status()["stub"]; !strings.HasPrefix(st, "connected") {
		t.Fatalf("not connected: %q", st)
	}
	if c := doctorCheck(t, a, reg, "plugin.stub"); !c.OK || !strings.Contains(c.Detail, "version 1.2.3") || !strings.Contains(c.Detail, "token accepted") {
		t.Fatalf("doctor: %+v", c)
	}
	if c := doctorCheck(t, a, reg, "plugin.sandbox"); !c.OK || !strings.Contains(c.Detail, "off") {
		t.Fatalf("doctor, sandbox: %+v", c)
	}
	// A wrong token is named as such.
	os.WriteFile(tokenFile, []byte("someone-elses-token-0123456789abcdef\n"), 0o644)
	if c := doctorCheck(t, a, reg, "plugin.stub"); c.OK || !strings.Contains(c.Detail, "refused the token") {
		t.Fatalf("doctor, wrong token: %+v", c)
	}

	// Stopped: on the next start it is simply absent, with no error, and
	// its old integration is turned off rather than reported broken.
	srv.Close()
	a.watchDefaultPlugins(ctx)
	servers, _ = a.DB.MCPServers(ctx)
	if len(servers) != 1 || servers[0].Enabled {
		t.Fatalf("absent plugin still on: %+v", servers)
	}
	if c := doctorCheck(t, a, reg, "plugin.stub"); !c.OK || !strings.Contains(c.Detail, "not running") || !strings.Contains(c.Detail, "--profile stub") {
		t.Fatalf("doctor, stopped: %+v", c)
	}
	a.mu.Lock()
	errs := a.startErrs
	a.mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("an absent default plugin is an error: %v", errs)
	}
}

// A plugin whose address was set and doesn't answer is reported.
func TestExplicitPluginFailureReported(t *testing.T) {
	a, _, _, _ := setup(t)
	reg := plugin.MustNew(plugin.Spec{Name: "stub"})
	a.Plugins = reg
	a.Cfg.SetPluginEnv("THICKET_STUB_URL", "http://127.0.0.1:1")
	if !a.isPlugin("stub") {
		t.Fatal("not a plugin")
	}
	if c := doctorCheck(t, a, reg, "plugin.stub"); c.OK {
		t.Fatalf("doctor passed a plugin that isn't there: %+v", c)
	}
}

// Thicket starts normally with no plugins running.
func TestStartsWithNoPlugins(t *testing.T) {
	a, _, _, ctx := setup(t)
	a.Plugins = plugin.MustNew(plugin.Sandbox, plugin.Spec{Name: "stub", DefaultURL: "http://127.0.0.1:1"})
	if !a.ensurePlugins(ctx) {
		t.Fatal("ensurePlugins failed with nothing configured")
	}
	if servers, _ := a.DB.MCPServers(ctx); len(servers) != 0 {
		t.Fatalf("integrations without plugins: %+v", servers)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(cctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

// Kid sprigs can't be given a plugin whose policy keeps kids out, even
// one read-only tool at a time, and aren't offered its tools.
func TestKidSprigsRefusedPlugins(t *testing.T) {
	h := setupHousehold(t)
	if _, err := h.a.Engine.KidAccess(h.ctx, h.owner, "bramble", []string{"mcp:sandbox/read_file"}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "can't use the sandbox plugin") {
		t.Fatalf("a kid sprig was given the sandbox: %v", err)
	}
	if _, err := h.a.Engine.KidAccess(h.ctx, h.owner, "bramble", []string{"mcp:ha/get_state"}, nil, nil, nil); err != nil {
		t.Fatalf("an ordinary read-only tool was refused: %v", err)
	}
}
