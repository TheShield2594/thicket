// Package sandbox is the server inside the thicket-sandbox container: a
// small Linux computer sprigs can use. It speaks MCP over HTTP, so Thicket
// connects to it like any integration, and every command goes through
// Thicket's approvals and permission levels.
//
// The container is the boundary: it has its own filesystem and a
// persistent /workspace, and no access to Thicket's data or keys. This
// server only checks the bearer token Thicket shares with it and keeps
// each command bounded in time and output.
package sandbox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	protocolVersion = "2025-06-18"
	defaultTimeout  = 60 * time.Second
	maxTimeout      = 10 * time.Minute
	maxOutput       = 16 << 10 // per stream, the end kept
	maxReadFile     = 256 << 10
	maxWriteFile    = 1 << 20
	maxListEntries  = 500
	maxConcurrent   = 4
)

type Server struct {
	// Workdir is where relative paths and commands start (/workspace).
	Workdir string
	// TokenFile holds the bearer token Thicket writes. It is read until
	// it exists, so the sandbox can start before Thicket.
	TokenFile string
	// Shell runs commands (default bash -lc).
	Shell []string

	mu    sync.Mutex
	token string
	slots chan struct{}
	once  sync.Once
}

func (s *Server) init() {
	s.once.Do(func() {
		s.slots = make(chan struct{}, maxConcurrent)
		if len(s.Shell) == 0 {
			s.Shell = []string{"bash", "-lc"}
		}
	})
}

func (s *Server) currentToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" {
		if b, err := os.ReadFile(s.TokenFile); err == nil {
			s.token = strings.TrimSpace(string(b))
		}
	}
	return s.token
}

// Handler serves MCP at /mcp and a health check at /health.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if s.currentToken() == "" {
			http.Error(w, "waiting for Thicket's token", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /mcp", s.serveMCP)
	return mux
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	tok := s.currentToken()
	if tok == "" {
		http.Error(w, "waiting for Thicket's token", http.StatusServiceUnavailable)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		// Thicket may have made a new token: read the file again.
		s.mu.Lock()
		s.token = ""
		s.mu.Unlock()
		tok = s.currentToken()
	}
	if tok == "" || subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="thicket-sandbox"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeRPC(w, json.RawMessage("null"), nil, &rpcErr{-32700, "parse error"})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted) // a notification
		return
	}
	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "thicket-sandbox", "version": "1"},
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, &rpcErr{-32602, "invalid params"})
			return
		}
		text, err := s.call(r.Context(), p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			writeRPC(w, req.ID, nil, &rpcErr{-32602, err.Error()})
			return
		}
		if err != nil {
			text = err.Error()
		}
		writeRPC(w, req.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil}, nil)
	default:
		writeRPC(w, req.ID, nil, &rpcErr{-32601, "method not found"})
	}
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcErr) {
	w.Header().Set("Content-Type", "application/json")
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	_ = json.NewEncoder(w).Encode(msg)
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]bool `json:"annotations"`
}

var tools = []tool{
	{"run_command", "Run a shell command (bash) on your own Linux computer, a Debian sandbox with git, curl, python3, jq and common tools, " +
		"and internet access. It starts in /workspace, which keeps your files between commands. Nothing of the user's is on it unless you put it there. " +
		"Returns the exit code, stdout and stderr (the last 16 KB of each). Processes it leaves running are stopped when it finishes.",
		json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"cwd":{"type":"string","description":"directory (default /workspace)"},"timeout_seconds":{"type":"integer","description":"default 60, at most 600"}},"required":["command"]}`),
		map[string]bool{"readOnlyHint": false, "destructiveHint": true}},
	{"read_file", "Read a text file on your sandbox computer (paths are relative to /workspace unless absolute). Up to 256 KB.",
		json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		map[string]bool{"readOnlyHint": true}},
	{"write_file", "Write a text file on your sandbox computer, creating folders as needed (relative to /workspace unless absolute). Replaces the file. Up to 1 MB.",
		json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		map[string]bool{"readOnlyHint": false, "destructiveHint": true}},
	{"list_files", "List a folder on your sandbox computer (default /workspace): names, sizes, folders marked with /.",
		json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		map[string]bool{"readOnlyHint": true}},
}

var errUnknownTool = errors.New("unknown tool")

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout_seconds"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("arguments: %v", err)
		}
	}
	switch name {
	case "run_command":
		return s.run(ctx, a.Command, a.Cwd, a.Timeout)
	case "read_file":
		return s.readFile(a.Path)
	case "write_file":
		return s.writeFile(a.Path, a.Content)
	case "list_files":
		return s.list(a.Path)
	}
	return "", fmt.Errorf("%w %q", errUnknownTool, name)
}

func (s *Server) path(p string) string {
	if p == "" {
		return s.Workdir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Workdir, p)
	}
	return filepath.Clean(p)
}

func (s *Server) run(ctx context.Context, command, cwd string, timeoutSec int) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is required")
	}
	timeout := defaultTimeout
	if timeoutSec > 0 {
		timeout = min(time.Duration(timeoutSec)*time.Second, maxTimeout)
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	dir := s.path(cwd)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("no folder %s", dir)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr := &tailBuffer{max: maxOutput}, &tailBuffer{max: maxOutput}
	code, err := runGroup(ctx, append(append([]string{}, s.Shell...), command), dir, stdout, stderr)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err != nil && !timedOut {
		return "", err
	}
	var b strings.Builder
	if timedOut {
		fmt.Fprintf(&b, "Stopped after %s (timeout).\n", timeout)
	} else {
		fmt.Fprintf(&b, "Exit code %d.\n", code)
	}
	if out := stdout.String(); out != "" {
		b.WriteString("\nstdout:\n" + out)
	}
	if out := stderr.String(); out != "" {
		b.WriteString("\nstderr:\n" + out)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte{}, t.buf[len(t.buf)-t.max:]...)
		t.dropped = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.ToValidUTF8(string(t.buf), "�")
	if t.dropped {
		s = "[earlier output cut]\n" + s
	}
	return s
}

func (s *Server) readFile(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	full := s.path(p)
	f, err := os.Open(full)
	if err != nil {
		return "", cleanErr(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", cleanErr(err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a folder; use list_files", full)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReadFile+1))
	if err != nil {
		return "", cleanErr(err)
	}
	cut := ""
	if len(b) > maxReadFile {
		b, cut = b[:maxReadFile], fmt.Sprintf("\n[file truncated: %d bytes in all]", st.Size())
	}
	if !utf8.Valid(b) && !utf8.Valid(b[:max(0, len(b)-3)]) {
		return "", fmt.Errorf("%s isn't text (%d bytes); use run_command to inspect it", full, st.Size())
	}
	return string(b) + cut, nil
}

func (s *Server) writeFile(p, content string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if len(content) > maxWriteFile {
		return "", fmt.Errorf("content is too large (at most %d bytes)", maxWriteFile)
	}
	full := s.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", cleanErr(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", cleanErr(err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(content), full), nil
}

func (s *Server) list(p string) (string, error) {
	full := s.path(p)
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", cleanErr(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d entries)\n", full, len(entries))
	for i, e := range entries {
		if i == maxListEntries {
			fmt.Fprintf(&b, "… and %d more\n", len(entries)-i)
			break
		}
		info, err := e.Info()
		switch {
		case err != nil:
			fmt.Fprintf(&b, "%s\n", e.Name())
		case e.IsDir():
			fmt.Fprintf(&b, "%s/\n", e.Name())
		case info.Mode()&fs.ModeSymlink != 0:
			fmt.Fprintf(&b, "%s (link)\n", e.Name())
		default:
			fmt.Fprintf(&b, "%s  %d bytes\n", e.Name(), info.Size())
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func cleanErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %v", pe.Path, pe.Err)
	}
	return err
}
