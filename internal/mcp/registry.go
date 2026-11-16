package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// Expander substitutes {{secret:NAME}} placeholders.
type Expander interface {
	Expand(ctx context.Context, s string) (string, error)
}

// ServerTool is a tool offered by a connected server.
type ServerTool struct {
	Server string
	Tool   Tool
}

// FuncName is the model-facing function name.
func (t ServerTool) FuncName() string { return FuncName(t.Server, t.Tool.Name) }

var unsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// FuncName maps a server/tool pair to a model-facing function name,
// mcp_<server>__<tool>. Server names can't contain "__", so the split is
// unambiguous. If the tool name had to be sanitised or the result is too
// long, a short hash of the exact pair is appended so distinct tools never
// collide (and the name stays stable for approval policies).
func FuncName(server, tool string) string {
	clean := unsafe.ReplaceAllString(tool, "_")
	n := "mcp_" + server + "__" + clean
	if clean == tool && len(n) <= 64 {
		return n
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(n) > 64-len(suffix) {
		n = n[:64-len(suffix)]
	}
	return n + suffix
}

type conn struct {
	client *Client
	tools  []Tool
	err    error
	at     time.Time
}

// Registry connects to the MCP servers stored in the database.
type Registry struct {
	DB      *store.Store
	Secrets Expander
	Log     *slog.Logger

	mu    sync.Mutex
	conns map[string]*conn
	gen   map[string]uint64 // bumped on every (re)connect or removal
}

func NewRegistry(db *store.Store, sec Expander, log *slog.Logger) *Registry {
	return &Registry{DB: db, Secrets: sec, Log: log, conns: map[string]*conn{}, gen: map[string]uint64{}}
}

func (r *Registry) dial(ctx context.Context, s store.MCPServer) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch s.Transport {
	case "stdio":
		env := map[string]string{}
		for k, v := range s.Env {
			x, err := r.Secrets.Expand(ctx, v)
			if err != nil {
				return nil, err
			}
			env[k] = x
		}
		return DialStdio(ctx, s.Target, s.Args, env)
	case "http":
		hdr := map[string]string{}
		for k, v := range s.Headers {
			x, err := r.Secrets.Expand(ctx, v)
			if err != nil {
				return nil, err
			}
			hdr[k] = x
		}
		return DialHTTP(ctx, s.Target, hdr)
	}
	return nil, fmt.Errorf("unknown transport %q", s.Transport)
}

// Connect (re)connects one server and lists its tools.
func (r *Registry) Connect(ctx context.Context, s store.MCPServer) ([]Tool, error) {
	c, err := r.connect(ctx, s)
	if err != nil {
		return nil, err
	}
	return c.tools, nil
}

// connect dials a server and publishes the connection, unless the server
// was removed or reconnected by someone else while we were dialling.
func (r *Registry) connect(ctx context.Context, s store.MCPServer) (*conn, error) {
	r.mu.Lock()
	r.gen[s.Name]++
	myGen := r.gen[s.Name]
	if old := r.conns[s.Name]; old != nil && old.client != nil {
		old.client.Close()
	}
	delete(r.conns, s.Name)
	r.mu.Unlock()

	c, err := r.dial(ctx, s)
	var tools []Tool
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		tools, err = c.ListTools(lctx)
		cancel()
		if err != nil {
			c.Close()
			c = nil
		}
	}
	cn := &conn{client: c, tools: tools, err: err, at: time.Now()}
	r.mu.Lock()
	if r.gen[s.Name] != myGen {
		r.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return nil, fmt.Errorf("integration %q was removed or reconnected while connecting", s.Name)
	}
	r.conns[s.Name] = cn
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", s.Name, err)
	}
	r.Log.Info("mcp server connected", "server", s.Name, "tools", len(tools))
	return cn, nil
}

// ConnectAll connects every enabled server, returning per-server errors.
func (r *Registry) ConnectAll(ctx context.Context) map[string]error {
	servers, err := r.DB.MCPServers(ctx)
	if err != nil {
		return map[string]error{"*": err}
	}
	errs := map[string]error{}
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if _, err := r.Connect(ctx, s); err != nil {
			errs[s.Name] = err
		}
	}
	return errs
}

// Disconnect drops a server.
func (r *Registry) Disconnect(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen[name]++ // invalidates any connection attempt in flight
	if c := r.conns[name]; c != nil && c.client != nil {
		c.client.Close()
	}
	delete(r.conns, name)
}

// Tools lists every tool from connected servers.
func (r *Registry) Tools() []ServerTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ServerTool
	for name, c := range r.conns {
		for _, t := range c.tools {
			out = append(out, ServerTool{Server: name, Tool: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FuncName() < out[j].FuncName() })
	return out
}

// Status describes each server's connection state.
func (r *Registry) Status() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for name, c := range r.conns {
		if c.err != nil {
			out[name] = "error: " + c.err.Error()
		} else {
			out[name] = fmt.Sprintf("connected, %d tools", len(c.tools))
		}
	}
	return out
}

// Call invokes a tool. retrySafe says whether the call may be replayed
// after a transport failure: only for read-only tools, since a dropped
// connection doesn't mean the server didn't act.
func (r *Registry) Call(ctx context.Context, server, tool string, args json.RawMessage, retrySafe bool) (*CallResult, error) {
	r.mu.Lock()
	c := r.conns[server]
	r.mu.Unlock()
	var client *Client
	if c != nil {
		client = c.client
	}
	if client == nil {
		var err error
		if client, err = r.reconnect(ctx, server); err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := client.CallTool(cctx, tool, args)
	var rpcErr *RPCError
	if err == nil || errors.As(err, &rpcErr) || ctx.Err() != nil {
		return res, err
	}
	if !retrySafe {
		return nil, fmt.Errorf("the connection to %s failed mid-call (%v), so I can't tell whether it took effect; check before trying again", server, err)
	}
	client, rerr := r.reconnect(ctx, server)
	if rerr != nil {
		return nil, err
	}
	cctx2, cancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel2()
	return client.CallTool(cctx2, tool, args)
}

// reconnect connects a configured server and returns its client directly,
// so a concurrent replacement can't leave the caller holding nothing.
func (r *Registry) reconnect(ctx context.Context, server string) (*Client, error) {
	servers, err := r.DB.MCPServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Name == server && s.Enabled {
			c, err := r.connect(ctx, s)
			if err != nil {
				return nil, err
			}
			return c.client, nil
		}
	}
	return nil, fmt.Errorf("integration %q is not configured", server)
}

func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		if c.client != nil {
			c.client.Close()
		}
	}
	r.conns = map[string]*conn{}
}
