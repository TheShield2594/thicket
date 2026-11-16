// Package mcp is a small Model Context Protocol client: enough to list and
// call tools over stdio or Streamable HTTP. No SDK dependency.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const protocolVersion = "2025-06-18"

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
		DestructiveHint *bool `json:"destructiveHint,omitempty"`
	} `json:"annotations"`
}

// ReadOnly reports whether the server says this tool has no side effects.
func (t Tool) ReadOnly() bool {
	return t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

type CallResult struct {
	Text    string
	IsError bool
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is any incoming message. A response has an id and no
// method; a server request has both; a notification has only a method.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// responseID returns the id of a response to one of our requests.
func (m *rpcResponse) responseID() (int64, bool) {
	if m.Method != "" || len(m.ID) == 0 {
		return 0, false
	}
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return 0, false
	}
	return id, true
}

// RPCError is an error the server returned for a request (as opposed to a
// transport failure).
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("%s: %s (code %d)", e.Method, e.Message, e.Code)
}

type transport interface {
	// roundTrip sends a request and waits for the response with the same id.
	roundTrip(ctx context.Context, req rpcRequest) (*rpcResponse, error)
	notify(ctx context.Context, req rpcRequest) error
	close() error
}

type Client struct {
	t      transport
	nextID atomic.Int64
}

// DialStdio starts command and speaks MCP over its stdin/stdout.
func DialStdio(ctx context.Context, command string, args []string, env map[string]string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	st := &stdio{cmd: cmd, in: stdin, wsem: make(chan struct{}, 1), pending: map[int64]chan *rpcResponse{}, done: make(chan struct{})}
	go st.readLoop(stdout)
	c := &Client{t: st}
	if err := c.initialize(ctx); err != nil {
		st.close()
		return nil, err
	}
	return c, nil
}

// DialHTTP connects to a Streamable HTTP MCP endpoint.
func DialHTTP(ctx context.Context, url string, headers map[string]string) (*Client, error) {
	h := &httpT{url: url, headers: headers, http: &http.Client{Timeout: 90 * time.Second}}
	c := &Client{t: h}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	resp, err := c.t.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

func (c *Client) initialize(ctx context.Context) error {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "thicket", "version": "1"},
	}, &res)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return c.t.notify(ctx, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for i := 0; i < 20; i++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, p := range res.Content {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
			b.WriteString("\n")
		default:
			fmt.Fprintf(&b, "[%s content omitted]\n", p.Type)
		}
	}
	if b.Len() == 0 && len(res.StructuredContent) > 0 {
		b.Write(res.StructuredContent)
	}
	return &CallResult{Text: strings.TrimSpace(b.String()), IsError: res.IsError}, nil
}

func (c *Client) Close() error { return c.t.close() }

// --- stdio transport ---

type stdio struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	wsem      chan struct{} // write lock that can be abandoned on cancel
	mu        sync.Mutex
	pending   map[int64]chan *rpcResponse
	done      chan struct{}
	err       error
	closeOnce sync.Once
}

func (s *stdio) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var msg rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue // stray log output
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 {
				go s.answerServerRequest(msg)
			}
			continue // notifications are ignored
		}
		id, ok := msg.responseID()
		if !ok {
			continue
		}
		s.mu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- &msg
		}
	}
	s.mu.Lock()
	s.err = errors.New("MCP server process exited")
	s.mu.Unlock()
	close(s.done)
}

// answerServerRequest replies to requests the server sends us. We support
// ping (required by the spec); everything else is "method not found".
func (s *stdio) answerServerRequest(req rpcResponse) {
	reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if req.Method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = map[string]any{"code": -32601, "message": "method not supported by this client"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.write(ctx, reply)
}

func (s *stdio) send(ctx context.Context, req rpcRequest) error { return s.write(ctx, req) }

// write sends one message. If ctx ends while the write is blocked (the
// server stopped reading and the pipe is full), the process is killed so
// nothing stays stuck behind it.
func (s *stdio) write(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	select {
	case s.wsem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("MCP server process exited")
	}
	errc := make(chan error, 1)
	go func() {
		defer func() { <-s.wsem }()
		_, err := s.in.Write(append(b, '\n'))
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		s.close()
		return ctx.Err()
	}
}

func (s *stdio) roundTrip(ctx context.Context, req rpcRequest) (*rpcResponse, error) {
	ch := make(chan *rpcResponse, 1)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	s.pending[*req.ID] = ch
	s.mu.Unlock()
	if err := s.send(ctx, req); err != nil {
		s.mu.Lock()
		delete(s.pending, *req.ID)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, errors.New("MCP server process exited")
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, *req.ID)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *stdio) notify(ctx context.Context, req rpcRequest) error { return s.send(ctx, req) }

func (s *stdio) close() error {
	s.closeOnce.Do(func() {
		s.in.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		go s.cmd.Wait() // reap without blocking the caller
	})
	return nil
}

// --- Streamable HTTP transport ---

type httpT struct {
	url     string
	headers map[string]string
	http    *http.Client
	mu      sync.Mutex
	session string
}

func (h *httpT) post(ctx context.Context, req any) (*http.Response, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	hr.Header.Set("MCP-Protocol-Version", protocolVersion)
	for k, v := range h.headers {
		hr.Header.Set(k, v)
	}
	h.mu.Lock()
	if h.session != "" {
		hr.Header.Set("Mcp-Session-Id", h.session)
	}
	h.mu.Unlock()
	resp, err := h.http.Do(hr)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (h *httpT) roundTrip(ctx context.Context, req rpcRequest) (*rpcResponse, error) {
	resp, err := h.post(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, *req.ID, func(msg rpcResponse) { h.answer(ctx, msg) })
	}
	var out rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("unreadable MCP response: %w", err)
	}
	return &out, nil
}

// answer replies to a request the server sent on an SSE stream, as a
// separate POST (same session headers): ping gets {}, anything else
// "method not found".
func (h *httpT) answer(ctx context.Context, req rpcResponse) {
	reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if req.Method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = map[string]any{"code": -32601, "message": "method not supported by this client"}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if resp, err := h.post(cctx, reply); err == nil {
		resp.Body.Close()
	}
}

// readSSE reads an SSE stream until the response to id arrives. Requests
// the server sends meanwhile (e.g. ping) are passed to onRequest.
func readSSE(r io.Reader, id int64, onRequest func(rpcResponse)) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var data strings.Builder
	flush := func() *rpcResponse {
		defer data.Reset()
		if data.Len() == 0 {
			return nil
		}
		var resp rpcResponse
		if json.Unmarshal([]byte(data.String()), &resp) == nil {
			if resp.Method != "" && len(resp.ID) > 0 && onRequest != nil {
				go onRequest(resp)
				return nil
			}
			if got, ok := resp.responseID(); ok && got == id {
				return &resp
			}
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if r := flush(); r != nil {
				return r, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if r := flush(); r != nil {
		return r, nil
	}
	return nil, errors.New("MCP stream ended without a response")
}

func (h *httpT) notify(ctx context.Context, req rpcRequest) error {
	resp, err := h.post(ctx, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpT) close() error { return nil }
