package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/store"
)

// --- tokens (straight on the database; works while Thicket runs) ---

func apiTokenCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: thicket api-token create NAME [--user APP:ID] | list | revoke ID")
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("api-token create", flag.ContinueOnError)
		who := fs.String("user", "", "who the token acts as, as discord:ID or telegram:ID (default: the owner)")
		pos, err := parseFlags(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
			return errors.New("usage: thicket api-token create NAME [--user APP:ID]")
		}
		var u *store.User
		if *who == "" {
			u, err = db.Owner(ctx)
			if errors.Is(err, store.ErrNotFound) {
				return errors.New("there's no owner yet: DM the bot first, then create a token")
			}
		} else {
			app, id, ok := strings.Cut(*who, ":")
			if !ok {
				return errors.New("--user must look like discord:123 or telegram:456")
			}
			u, err = db.UserByExternal(ctx, app, id)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no Thicket user is %s", *who)
			}
		}
		if err != nil {
			return err
		}
		token, hash, err := api.NewToken()
		if err != nil {
			return err
		}
		id, err := db.CreateAPIToken(ctx, u.ID, pos[0], hash)
		if err != nil {
			return err
		}
		name := u.Name
		if name == "" {
			name = u.Channel + ":" + u.ExternalID
		}
		fmt.Fprintf(os.Stderr, "Token %d %q created, acting as %s (%s). It is shown only now; keep it like a password.\n", id, pos[0], name, u.Role)
		fmt.Println(token)
		return nil
	case "list":
		toks, err := db.ListAPITokens(ctx)
		if err != nil {
			return err
		}
		if len(toks) == 0 {
			fmt.Println("no API tokens")
		}
		for _, t := range toks {
			used := "never used"
			if !t.LastUsedAt.IsZero() {
				used = "last used " + t.LastUsedAt.Format("2006-01-02 15:04")
			}
			fmt.Printf("%4d  %-20s user=%d  created %s  %s\n", t.ID, t.Name, t.UserID, t.CreatedAt.Format("2006-01-02"), used)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: thicket api-token revoke ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		if err := db.RevokeAPIToken(ctx, id); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no token %d", id)
		} else if err != nil {
			return err
		}
		fmt.Printf("token %d revoked\n", id)
		return nil
	}
	return fmt.Errorf("unknown api-token command %q", args[0])
}

// --- client commands (through the running server) ---

type client struct {
	url, token string
	http       *http.Client
}

func newClient() (*client, error) {
	tok := strings.TrimSpace(os.Getenv("THICKET_API_TOKEN"))
	if f := os.Getenv("THICKET_API_TOKEN_FILE"); tok == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		return nil, errors.New("set THICKET_API_TOKEN (or THICKET_API_TOKEN_FILE) to a token from `thicket api-token create NAME`")
	}
	url := strings.TrimRight(os.Getenv("THICKET_API_URL"), "/")
	if url == "" {
		port := "8080"
		if _, p, err := net.SplitHostPort(loadConfig().HTTPAddr); err == nil && p != "" {
			port = p
		}
		url = "http://127.0.0.1:" + port
	}
	return &client{url: url, token: tok, http: &http.Client{Timeout: 4 * time.Minute}}, nil
}

func (c *client) do(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.url+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach Thicket at %s (is it running? set THICKET_API_URL if it's elsewhere): %w", c.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func askCmd(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	sprig := fs.String("sprig", "", "which sprig to ask (id or name; default: whoever you spoke to last)")
	conv := fs.String("conversation", "", "conversation name, to keep separate threads (default: \"default\")")
	asJSON := fs.Bool("json", false, "print the raw JSON response")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	text := strings.Join(pos, " ")
	if text == "" || text == "-" {
		b, err := readStdin()
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("usage: thicket ask [--sprig ID] [--conversation NAME] [--json] MESSAGE (or the message on stdin)")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var resp struct {
		Conversation string      `json:"conversation"`
		Replies      []api.Reply `json:"replies"`
	}
	if err := c.do(http.MethodPost, "/api/v1/messages", map[string]string{"text": text, "sprig": *sprig, "conversation": *conv}, &resp); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	for i, r := range resp.Replies {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s: %s\n", r.Sprig, r.Text)
	}
	return nil
}

func runJobCmd(args []string) error {
	fs := flag.NewFlagSet("run-job", flag.ContinueOnError)
	payload := fs.String("payload", "", "input for the run (\"-\" reads stdin)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thicket run-job JOB_ID [--payload TEXT|-]")
	}
	if _, err := strconv.ParseInt(pos[0], 10, 64); err != nil {
		return fmt.Errorf("JOB_ID must be a number (see `thicket jobs`)")
	}
	if *payload == "-" {
		b, err := readStdin()
		if err != nil {
			return err
		}
		*payload = string(b)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/v1/jobs/"+pos[0]+"/run", map[string]string{"payload": *payload}, nil); err != nil {
		return err
	}
	fmt.Println("started; the sprig will message you with the result")
	return nil
}

func jobsCmd() error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var resp struct {
		Jobs []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
			Schedule string `json:"schedule"`
			Sprig    string `json:"sprig"`
			Enabled  bool   `json:"enabled"`
			LastRun  string `json:"last_run"`
		} `json:"jobs"`
		Tasks []struct {
			ID        int64  `json:"id"`
			Sprig     string `json:"sprig"`
			Task      string `json:"task"`
			Status    string `json:"status"`
			StartedAt string `json:"started_at"`
		} `json:"tasks"`
	}
	if err := c.do(http.MethodGet, "/api/v1/jobs", nil, &resp); err != nil {
		return err
	}
	if len(resp.Jobs) == 0 {
		fmt.Println("no jobs")
	}
	for _, j := range resp.Jobs {
		when := j.Schedule
		if j.Kind == "webhook" {
			when = "webhook"
		}
		state := ""
		if !j.Enabled {
			state = " (paused)"
		}
		last := j.LastRun
		if last == "" {
			last = "never run"
		}
		fmt.Printf("%4d  %-24s %-14s by %-10s last: %s%s\n", j.ID, j.Name, when, j.Sprig, last, state)
	}
	if len(resp.Tasks) > 0 {
		fmt.Println("\nbackground tasks (newest first):")
	}
	for _, t := range resp.Tasks {
		task := t.Task
		if r := []rune(task); len(r) > 48 {
			task = string(r[:47]) + "…"
		}
		started := t.StartedAt
		if ts, err := time.Parse(time.RFC3339, t.StartedAt); err == nil {
			started = ts.Local().Format("Jan 2 15:04")
		}
		fmt.Printf("%4d  %-11s %-10s %s  %s\n", t.ID, t.Status, t.Sprig, started, task)
	}
	return nil
}

func stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	conv := fs.String("conversation", "", "the conversation to stop (default: \"default\")")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var resp struct {
		Reply bool     `json:"stopped_reply"`
		Jobs  []string `json:"stopped_jobs"`
	}
	if err := c.do(http.MethodPost, "/api/v1/stop", map[string]string{"conversation": *conv}, &resp); err != nil {
		return err
	}
	switch {
	case resp.Reply:
		fmt.Println("stopped the reply in progress")
	case len(resp.Jobs) > 0:
		fmt.Println("stopped jobs:", strings.Join(resp.Jobs, ", "))
	default:
		fmt.Println("nothing was running")
	}
	return nil
}

// maxStdin is the most read from stdin: the server's limits are lower, so
// anything bigger is refused here rather than silently cut short.
const maxStdin = 64 << 10

func readStdin() ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdin+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxStdin {
		return nil, fmt.Errorf("stdin is over %d KB; send less", maxStdin>>10)
	}
	return b, nil
}
