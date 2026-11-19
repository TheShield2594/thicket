package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// The sandbox is the thicket-sandbox container, sprigs' own computer. It
// is an ordinary MCP integration named "sandbox", set up from
// THICKET_SANDBOX_URL: no sprig can use it until the owner grants
// mcp:sandbox/*, and its commands ask for approval like any tool that
// changes things.
const (
	sandboxName   = "sandbox"
	sandboxSecret = "sandbox_token"
)

// ensureSandbox shares a token with the sandbox (through a file both
// containers mount) and keeps the integration's settings current.
func (a *App) ensureSandbox(ctx context.Context) {
	if a.Cfg.SandboxURL == "" {
		return
	}
	if err := a.setUpSandbox(ctx); err != nil {
		a.startErr("The sandbox computer isn't set up: " + shortErr(err))
	}
}

func (a *App) sandboxTokenFile() string {
	if a.Cfg.SandboxTokenFile != "" {
		return a.Cfg.SandboxTokenFile
	}
	return filepath.Join(a.Cfg.DataDir, "sandbox", "token")
}

func (a *App) setUpSandbox(ctx context.Context) error {
	path := a.sandboxTokenFile()
	tok := ""
	if b, err := os.ReadFile(path); err == nil {
		tok = strings.TrimSpace(string(b))
	}
	if len(tok) < 32 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		tok = base64.RawURLEncoding.EncodeToString(b)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("token folder: %w", err)
		}
		// Readable by the sandbox's own user: the file only unlocks the
		// sandbox itself.
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing the token: %w", err)
		}
	}
	if cur, _ := a.Secrets.Get(ctx, sandboxSecret); cur != tok {
		if err := a.Secrets.Put(ctx, sandboxSecret, tok); err != nil {
			return err
		}
	}
	want := store.MCPServer{Name: sandboxName, Transport: "http", Target: a.Cfg.SandboxURL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer {{secret:" + sandboxSecret + "}}"}, Enabled: true}
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name == sandboxName && s.Transport == want.Transport && s.Target == want.Target && maps.Equal(s.Headers, want.Headers) && s.Enabled {
			return nil
		}
	}
	return a.DB.PutMCPServer(ctx, want)
}

// retrySandbox waits for a sandbox that is still starting.
func (a *App) retrySandbox(ctx context.Context) error {
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name != sandboxName {
			continue
		}
		for i := 0; i < 6; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			if _, err = a.MCP.Connect(ctx, s); err == nil {
				return nil
			}
		}
		return err
	}
	return errors.New("no sandbox integration")
}
