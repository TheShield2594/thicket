package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/theshield2594/thicket/internal/api"
)

// `thicket mcp` is an MCP server on stdin/stdout, so an agent such as
// Claude Desktop, Claude Code or Cursor can talk to your sprigs. It is a
// thin client of the local API: it needs a token (THICKET_API_TOKEN) and
// can do what that token's person can do in chat, with one exception:
// it can never answer an approval prompt. Risky actions still wait for
// the person, on the dashboard or in a chat app.
//
// The tools are deliberately few: talk to a sprig, stop a reply, list
// sprigs and jobs, run a job. Settings, secrets, approvals, deleting and
// characters stay with the person.

const mcpDefaultConversation = "mcp"

// mcpVersions are the protocol versions this server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{"ask_sprig", "Send a message to one of the user's Thicket sprigs (AI companions with their own memory of the user, tools and roles) and get the reply. " +
		"Replies can take up to 3 minutes when the sprig uses tools. Without `sprig`, whoever answered last in the conversation replies. " +
		"Use `conversation` to keep separate threads (default \"mcp\"). If a reply asks for approval, tell the user: only they can approve, on Thicket's dashboard or in their chat app.",
		json.RawMessage(`{"type":"object","properties":{` +
			`"message":{"type":"string","description":"What to say to the sprig."},` +
			`"sprig":{"type":"string","description":"Sprig id or name (see list_sprigs). Optional."},` +
			`"conversation":{"type":"string","description":"Thread name: up to 40 lowercase letters, digits, - or _. Optional."}},` +
			`"required":["message"]}`)},
	{"list_sprigs", "List the user's sprigs: id, name, role, and which one is the host.",
		json.RawMessage(`{"type":"object","properties":{}}`)},
	{"stop", "Stop the sprig reply in progress in a conversation (or, if none is running, the user's running jobs).",
		json.RawMessage(`{"type":"object","properties":{"conversation":{"type":"string","description":"Thread name (default \"mcp\")."}}}`)},
	{"list_jobs", "List the user's scheduled and webhook jobs with their last run.",
		json.RawMessage(`{"type":"object","properties":{}}`)},
	{"run_job", "Run one of the user's jobs now (see list_jobs). The sprig messages the user with the result in their chat app.",
		json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"integer","description":"Job id."},` +
			`"payload":{"type":"string","description":"Optional input for this run, given to the sprig as outside data."}},` +
			`"required":["id"]}`)},
}

func mcpCmd() error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return serveMCP(os.Stdin, os.Stdout, c)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveMCP reads newline-delimited JSON-RPC from in until it closes.
// Tool calls run concurrently (a sprig can take minutes), so a stop or a
// ping still gets through.
func serveMCP(in io.Reader, out io.Writer, c *client) error {
	var mu sync.Mutex
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out.Write(append(b, '\n'))
	}
	reply := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{code, msg}})
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.Method == "" {
			fail(json.RawMessage("null"), -32700, "parse error")
			continue
		}
		notification := len(req.ID) == 0 || string(req.ID) == "null"
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			v := mcpVersions[0]
			for _, s := range mcpVersions {
				if s == p.ProtocolVersion {
					v = s
				}
			}
			reply(req.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "thicket", "version": version},
				"instructions": "Thicket is the user's self-hosted family of AI companions (sprigs). Use ask_sprig to talk to them; " +
					"they remember the user and have their own tools. You can't approve their risky actions: the user does that.",
			})
		case "ping":
			reply(req.ID, map[string]any{})
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				fail(req.ID, -32602, "invalid params")
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				text, err := callMCPTool(c, p.Name, p.Arguments)
				if errors.Is(err, errUnknownTool) {
					fail(id, -32602, err.Error())
					return
				}
				if err != nil {
					text = err.Error()
				}
				reply(id, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil})
			}(req.ID)
		default:
			if !notification { // notifications (initialized, cancelled) need no answer
				fail(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
	return sc.Err()
}

var errUnknownTool = errors.New("unknown tool")

func callMCPTool(c *client, name string, raw json.RawMessage) (string, error) {
	var args struct {
		Message      string          `json:"message"`
		Sprig        string          `json:"sprig"`
		Conversation string          `json:"conversation"`
		ID           json.RawMessage `json:"id"`
		Payload      string          `json:"payload"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("arguments must be a JSON object: %v", err)
		}
	}
	conv := args.Conversation
	if conv == "" {
		conv = mcpDefaultConversation
	}
	switch name {
	case "ask_sprig":
		if strings.TrimSpace(args.Message) == "" {
			return "", errors.New("message is required")
		}
		var resp struct {
			Conversation string      `json:"conversation"`
			Replies      []api.Reply `json:"replies"`
		}
		body := map[string]any{"text": args.Message, "sprig": args.Sprig, "conversation": conv, "no_approvals": true}
		if err := c.do(http.MethodPost, "/api/v1/messages", body, &resp); err != nil {
			return "", err
		}
		if len(resp.Replies) == 0 {
			return "(no reply)", nil
		}
		var b strings.Builder
		for i, r := range resp.Replies {
			if i > 0 {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "%s (%s): %s", r.Sprig, r.SprigID, r.Text)
		}
		return b.String(), nil
	case "list_sprigs", "list_jobs":
		path := "/api/v1/sprigs"
		if name == "list_jobs" {
			path = "/api/v1/jobs"
		}
		var out json.RawMessage
		if err := c.do(http.MethodGet, path, nil, &out); err != nil {
			return "", err
		}
		return string(out), nil
	case "stop":
		var out json.RawMessage
		if err := c.do(http.MethodPost, "/api/v1/stop", map[string]string{"conversation": conv}, &out); err != nil {
			return "", err
		}
		return string(out), nil
	case "run_job":
		id, err := strconv.ParseInt(strings.Trim(string(args.ID), `"`), 10, 64)
		if err != nil || id <= 0 {
			return "", errors.New("id must be a job id (see list_jobs)")
		}
		if err := c.do(http.MethodPost, "/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/run", map[string]string{"payload": args.Payload}, nil); err != nil {
			return "", err
		}
		return "Started. The sprig will message the user in their chat app with the result.", nil
	}
	return "", fmt.Errorf("%w %q", errUnknownTool, name)
}
