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
	// Exec: the server can run commands or programs (MCPServer.Exec).
	Exec bool
	// Local: the server is a process on Thicket's own machine (stdio), so
	// paths in its arguments are on that machine too.
	Local bool
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

// conn is one live connection. Calls hold a reference while they use the
// client; a replaced or removed connection is only closed once the last
// in-flight call releases it.
type conn struct {
	client    *Client
	tools     []Tool
	err       error
	at        time.Time
	server    string
	transport string
	exec      bool
	refs      int  // guarded by Registry.mu
	retired   bool // guarded by Registry.mu
}

// connKey names a connection: the server's own (for listing tools, and
// for every call to a remote server), or one sprig's process of a stdio
// server, which runs in that sprig's folder.
func connKey(server, sprig string) string {
	if sprig == "" {
		return server
	}
	return server + "\x00" + sprig
}

// retireLocked marks c as replaced and closes it if nothing is using it.
// r.mu must be held.
func (r *Registry) retireLocked(c *conn) {
	if c == nil || c.retired {
		return
	}
	c.retired = true
	if c.refs == 0 && c.client != nil {
		c.client.Close()
	}
}

// acquireLocked takes a reference on c for a call. r.mu must be held.
func acquireLocked(c *conn) *conn {
	if c == nil || c.client == nil || c.retired {
		return nil
	}
	c.refs++
	return c
}

// release drops a call's reference, closing a retired connection once the
// last call is done with it.
func (r *Registry) release(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.refs--
	if c.retired && c.refs == 0 && c.client != nil {
		c.client.Close()
	}
}

// Registry connects to the MCP servers stored in the database.
type Registry struct {
	DB      *store.Store
	Secrets Expander
	Log     *slog.Logger
	// OAuth supplies tokens for servers signed in with OAuth.
	OAuth *OAuth
	// RetryBackoff is the first wait before retrying a read-only call
	// (doubles each time); 0 means 500ms.
	RetryBackoff time.Duration
	// Folder returns the working folder for a stdio server's process: the
	// sprig's own folder, or (sprig "") a neutral one for listing tools.
	// It returns "" to use Thicket's own. Home is HOME for every stdio
	// server (caches, config), shared by all of them.
	Folder func(sprig string) (string, error)
	Home   string

	mu    sync.Mutex
	conns map[string]*conn  // by connKey
	gen   map[string]uint64 // by connKey: bumped on every (re)connect or removal
	sgen  map[string]uint64 // by server: bumped when a server is removed or replaced
	// blocked servers are never connected (Block).
	blocked map[string]bool
}

// ErrBlocked means a server is kept from connecting (Block).
var ErrBlocked = errors.New("this integration is turned off")

// Block keeps a server from connecting, whatever its stored settings say,
// and drops any connection it has: for a plugin turned off whose stored
// integration couldn't be turned off.
func (r *Registry) Block(name string) {
	r.mu.Lock()
	if r.blocked == nil {
		r.blocked = map[string]bool{}
	}
	r.blocked[name] = true
	r.mu.Unlock()
	r.Disconnect(name)
}

func NewRegistry(db *store.Store, sec Expander, log *slog.Logger) *Registry {
	return &Registry{DB: db, Secrets: sec, Log: log, conns: map[string]*conn{}, gen: map[string]uint64{}, sgen: map[string]uint64{}}
}

func (r *Registry) dial(ctx context.Context, s store.MCPServer, sprig string) (*Client, error) {
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
		dir := ""
		if r.Folder != nil {
			var err error
			if dir, err = r.Folder(sprig); err != nil {
				return nil, err
			}
		}
		return DialStdioIn(ctx, dir, r.Home, s.Target, s.Args, env)
	case "http":
		hdr := map[string]string{}
		var token TokenFunc
		for k, v := range s.Headers {
			if v == OAuthHeader {
				if r.OAuth == nil {
					return nil, errors.New("OAuth isn't available")
				}
				name := s.Name
				token = func(ctx context.Context, force bool) (string, error) { return r.OAuth.AccessToken(ctx, name, force) }
				continue
			}
			x, err := r.Secrets.Expand(ctx, v)
			if err != nil {
				return nil, err
			}
			hdr[k] = x
		}
		return dialHTTP(ctx, s.Target, hdr, token)
	}
	return nil, fmt.Errorf("unknown transport %q", s.Transport)
}

// Connect (re)connects one server and lists its tools. A stdio server's
// per-sprig processes are restarted too, on their next call, so they
// pick up its new settings.
func (r *Registry) Connect(ctx context.Context, s store.MCPServer) ([]Tool, error) {
	r.mu.Lock()
	r.retireSprigsLocked(s.Name)
	r.mu.Unlock()
	c, err := r.connect(ctx, s, "", false)
	if err != nil {
		return nil, err
	}
	return c.tools, nil
}

// retireSprigsLocked drops a server's per-sprig processes. r.mu must be
// held.
func (r *Registry) retireSprigsLocked(server string) {
	r.sgen[server]++ // invalidates per-sprig connection attempts in flight
	for k, c := range r.conns {
		if c.server == server && k != server {
			r.gen[k]++
			r.retireLocked(c)
			delete(r.conns, k)
		}
	}
}

// connect dials a server (for one sprig, for a stdio server, or its own
// connection when sprig is "") and publishes the connection, unless the
// server was removed or reconnected by someone else while we were
// dialling.
func (r *Registry) connect(ctx context.Context, s store.MCPServer, sprig string, hold bool) (*conn, error) {
	if s.Transport != "stdio" {
		sprig = "" // one connection serves every sprig
	}
	key := connKey(s.Name, sprig)
	r.mu.Lock()
	if r.blocked[s.Name] {
		r.mu.Unlock()
		return nil, ErrBlocked
	}
	r.gen[key]++
	myGen, mySGen := r.gen[key], r.sgen[s.Name]
	r.retireLocked(r.conns[key])
	delete(r.conns, key)
	r.mu.Unlock()

	c, err := r.dial(ctx, s, sprig)
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
	cn := &conn{client: c, tools: tools, err: err, at: time.Now(), server: s.Name, transport: s.Transport, exec: s.Exec}
	r.mu.Lock()
	if r.gen[key] != myGen || (sprig != "" && r.sgen[s.Name] != mySGen) {
		r.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return nil, fmt.Errorf("integration %q was removed or reconnected while connecting", s.Name)
	}
	r.conns[key] = cn
	if hold && err == nil {
		cn.refs++ // taken under the same lock that publishes it
	}
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

// Disconnect drops a server, and every sprig's process of it.
func (r *Registry) Disconnect(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen[name]++ // invalidates any connection attempt in flight
	r.retireLocked(r.conns[name])
	delete(r.conns, name)
	r.retireSprigsLocked(name)
}

// Tools lists every tool from connected servers.
func (r *Registry) Tools() []ServerTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ServerTool
	for name, c := range r.conns {
		if name != c.server && c.server != "" {
			continue // a sprig's own process: listed through the server's
		}
		for _, t := range c.tools {
			out = append(out, ServerTool{Server: name, Tool: t, Exec: c.exec, Local: c.transport == "stdio"})
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
		if name != c.server && c.server != "" {
			continue
		}
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
	return r.call(ctx, server, "", tool, args, retrySafe, false, true)
}

// ErrExec means a call reached an integration marked exec for a sprig
// that may not use one.
var ErrExec = errors.New("runs commands, and this sprig's exec access is off")

// CallAs invokes a tool for a sprig. A stdio server runs a process of its
// own for each sprig, in that sprig's folder; other servers share one
// connection. With allowExec false, a call is refused if the connection
// that would serve it (after any reconnect) is marked exec.
func (r *Registry) CallAs(ctx context.Context, sprig, server, tool string, args json.RawMessage, retrySafe, allowExec bool) (*CallResult, error) {
	return r.call(ctx, server, sprig, tool, args, retrySafe, false, allowExec)
}

// CallReadOnlyAs is CallReadOnly for a sprig, as CallAs.
func (r *Registry) CallReadOnlyAs(ctx context.Context, sprig, server, tool string, args json.RawMessage, allowExec bool) (*CallResult, error) {
	return r.call(ctx, server, sprig, tool, args, true, true, allowExec)
}

// CallReadOnly calls a tool only if the very connection used lists it as
// read-only (for automatic callers, like trigger polls, that must never
// run a tool that changes things). It's retried like any read-only call.
func (r *Registry) CallReadOnly(ctx context.Context, server, tool string, args json.RawMessage) (*CallResult, error) {
	return r.call(ctx, server, "", tool, args, true, true, true)
}

// ErrNotReadOnly means a tool isn't (or is no longer) listed as read-only.
var ErrNotReadOnly = errors.New("is not listed as read-only")

func (r *Registry) call(ctx context.Context, server, sprig, tool string, args json.RawMessage, retrySafe, onlyReadOnly, allowExec bool) (*CallResult, error) {
	r.mu.Lock()
	var c *conn
	if b := r.conns[server]; sprig != "" && b != nil && b.transport == "stdio" {
		c = acquireLocked(r.conns[connKey(server, sprig)])
	} else if sprig == "" || b != nil {
		c = acquireLocked(b)
	}
	r.mu.Unlock()
	if c == nil {
		var err error
		if c, err = r.reconnect(ctx, server, sprig); err != nil {
			return nil, err
		}
	}
	if c.exec && !allowExec {
		r.release(c)
		return nil, fmt.Errorf("%s %w", server, ErrExec)
	}
	if onlyReadOnly && !listsReadOnly(c.tools, tool) {
		r.release(c)
		return nil, fmt.Errorf("%s → %s %w", server, tool, ErrNotReadOnly)
	}
	res, err := r.callOn(ctx, c, tool, args)
	var rpcErr *RPCError
	if err == nil || errors.As(err, &rpcErr) || ctx.Err() != nil {
		return res, err
	}
	if !retrySafe {
		return nil, fmt.Errorf("the connection to %s failed mid-call (%v), so I can't tell whether it took effect; check before trying again", server, err)
	}
	// Read-only: reconnect and try again, with backoff, a few times.
	backoff := r.RetryBackoff
	if backoff == 0 {
		backoff = 500 * time.Millisecond
	}
	for attempt := 0; attempt < callRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		c2, rerr := r.reconnect(ctx, server, sprig)
		if rerr != nil {
			err = rerr
			continue
		}
		if c2.exec && !allowExec {
			r.release(c2)
			return nil, fmt.Errorf("%s %w", server, ErrExec)
		}
		// The server may have changed on reconnect: replay only if this
		// connection still lists the tool as read-only.
		if !listsReadOnly(c2.tools, tool) {
			r.release(c2)
			return nil, fmt.Errorf("the connection to %s dropped mid-call, and after reconnecting %s is no longer listed as read-only, so I didn't retry it", server, tool)
		}
		res, err = r.callOn(ctx, c2, tool, args)
		if err == nil || errors.As(err, &rpcErr) || ctx.Err() != nil {
			return res, err
		}
	}
	return nil, err
}

// listsReadOnly reports whether tools has name, marked read-only.
func listsReadOnly(tools []Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return t.ReadOnly()
		}
	}
	return false
}

// callRetries is how many times a read-only call is retried after a
// transport failure.
const callRetries = 2

// callOn runs one call on a held connection and releases it afterwards.
func (r *Registry) callOn(ctx context.Context, c *conn, tool string, args json.RawMessage) (*CallResult, error) {
	defer r.release(c)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return c.client.CallTool(cctx, tool, args)
}

// reconnect connects a configured server and returns the new connection
// already held for the caller, so a concurrent replacement can't close it
// before (or while) the caller uses it.
func (r *Registry) reconnect(ctx context.Context, server, sprig string) (*conn, error) {
	servers, err := r.DB.MCPServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Name == server && s.Enabled {
			return r.connect(ctx, s, sprig, true)
		}
	}
	return nil, fmt.Errorf("integration %q is not configured", server)
}

func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		r.retireLocked(c)
	}
	r.conns = map[string]*conn{}
}
