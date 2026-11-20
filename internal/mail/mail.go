// Package mail is Thicket core's side of the thicket-mail plugin
// (plugins/mail, docs/mail.md): the client for the plugin's admin API
// (accounts, events, the dashboard's mailbox, a person's send or
// discard) and the rules core keeps itself (allowlist entries, account
// settings). The plugin speaks JMAP, IMAP and SMTP; core never does.
// The contract is plugins/mail/PROTOCOL.md.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Plugin is the plugin's integration name (mcp:mail/*).
const Plugin = "mail"

// Settings are an address's non-secret connection settings, kept as JSON
// in the store.
type Settings struct {
	Username string `json:"username,omitempty"`
	// JMAP
	SessionURL string `json:"session_url,omitempty"`
	// IMAP/SMTP
	IMAPHost     string `json:"imap_host,omitempty"`
	IMAPPort     int    `json:"imap_port,omitempty"`
	IMAPSecurity string `json:"imap_security,omitempty"` // tls | starttls
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty"`
	SMTPSecurity string `json:"smtp_security,omitempty"` // tls | starttls
}

// Check validates settings for a backend. Plaintext is never allowed.
func (s Settings) Check(backend string) error {
	switch backend {
	case "jmap":
		u, err := url.Parse(s.SessionURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("the JMAP session URL must be an https:// address (for Fastmail: https://api.fastmail.com/jmap/session)")
		}
	case "imap":
		if s.IMAPHost == "" || s.SMTPHost == "" {
			return errors.New("IMAP needs imap_host and smtp_host")
		}
		if s.Username == "" {
			return errors.New("IMAP needs a username (usually the address)")
		}
		for _, sec := range []string{s.IMAPSecurity, s.SMTPSecurity} {
			if sec != "tls" && sec != "starttls" {
				return errors.New(`security must be "tls" or "starttls": mail is never sent or read in plaintext`)
			}
		}
		for _, p := range []int{s.IMAPPort, s.SMTPPort} {
			if p < 1 || p > 65535 {
				return errors.New("IMAP and SMTP ports must be between 1 and 65535")
			}
		}
	default:
		return errors.New(`backend must be "jmap" or "imap"`)
	}
	return nil
}

// Defaults fills in the usual ports for the security chosen.
func (s *Settings) Defaults() {
	if s.IMAPSecurity == "" {
		s.IMAPSecurity = "tls"
	}
	if s.SMTPSecurity == "" {
		s.SMTPSecurity = "tls"
	}
	if s.IMAPPort == 0 {
		s.IMAPPort = map[string]int{"tls": 993, "starttls": 143}[s.IMAPSecurity]
	}
	if s.SMTPPort == 0 {
		s.SMTPPort = map[string]int{"tls": 465, "starttls": 587}[s.SMTPSecurity]
	}
}

// Address checks and normalises one email address (lower case).
func Address(s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s && "<"+a.Address+">" != s {
		return "", fmt.Errorf("%q isn't an email address", s)
	}
	return strings.ToLower(a.Address), nil
}

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// AllowEntry checks and normalises an allowlist entry: an exact address
// (shop@vendor.example) or a whole domain (@vendor.example; not its
// subdomains).
func AllowEntry(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if d, ok := strings.CutPrefix(s, "@"); ok {
		if !domainRE.MatchString(d) {
			return "", fmt.Errorf("%q isn't a domain", d)
		}
		return s, nil
	}
	if strings.ContainsAny(s, "*?") {
		return "", errors.New("no wildcards: give an exact address or @domain")
	}
	return Address(s)
}

// Allowed reports whether addr is on the list (exact address, or its
// domain as @domain). It is what the plugin checks too; core uses it to
// show the person which recipients would need them.
func Allowed(list []string, addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return false
	}
	for _, e := range list {
		if e == addr || e == addr[at:] {
			return true
		}
	}
	return false
}

// Account is what core pushes to the plugin (PUT /v1/accounts).
type Account struct {
	Sprig         string  `json:"sprig"`
	Address       string  `json:"address"`
	Name          string  `json:"name,omitempty"`
	Backend       string  `json:"backend"`
	JMAP          *Server `json:"jmap,omitempty"`
	IMAP          *Server `json:"imap,omitempty"`
	SMTP          *Server `json:"smtp,omitempty"`
	Username      string  `json:"username"`
	Password      string  `json:"password"`
	RetentionDays int     `json:"retention_days"`
	SendLimit     int     `json:"send_limit"`
}

// Server is one server's address.
type Server struct {
	SessionURL string `json:"session_url,omitempty"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Security   string `json:"security,omitempty"`
}

// AccountState is the plugin's answer about one account.
type AccountState struct {
	Sprig string `json:"sprig"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// Event is something that happened in a mailbox. Bodies are never in it.
type Event struct {
	Seq       int64     `json:"seq"`
	Sprig     string    `json:"sprig"`
	Kind      string    `json:"kind"` // received | sent | drafted | discarded | archived | error
	At        time.Time `json:"at"`
	From      string    `json:"from,omitempty"`
	To        []string  `json:"to,omitempty"`
	Cc        []string  `json:"cc,omitempty"`
	Bcc       []string  `json:"bcc,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Size      int64     `json:"size,omitempty"`
	MessageID string    `json:"message_id,omitempty"`
	DraftID   string    `json:"draft_id,omitempty"`
	OTP       bool      `json:"otp,omitempty"`
	Approved  bool      `json:"approved,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Events is one long poll's answer.
type Events struct {
	Epoch  string  `json:"epoch"`
	Next   int64   `json:"next"`
	Events []Event `json:"events"`
}

// Summary is one message in a mailbox listing.
type Summary struct {
	ID          string   `json:"id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Subject     string   `json:"subject"`
	Date        string   `json:"date"`
	Size        int64    `json:"size"`
	Preview     string   `json:"preview"`
	Attachments int      `json:"attachments"`
	Unread      bool     `json:"unread"`
	Draft       bool     `json:"draft"`
}

// Message is one message as text (HTML converted by the plugin).
type Message struct {
	ID          string       `json:"id"`
	From        string       `json:"from"`
	To          []string     `json:"to"`
	Cc          []string     `json:"cc"`
	Bcc         []string     `json:"bcc"`
	Subject     string       `json:"subject"`
	Date        string       `json:"date"`
	Size        int64        `json:"size"`
	Text        string       `json:"text"`
	Draft       bool         `json:"draft"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment is listed, never opened by Thicket.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// Drafted is a tool result's _meta "thicket/drafted".
type Drafted struct {
	DraftID string   `json:"draft_id"`
	From    string   `json:"from"`
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Bcc     []string `json:"bcc"`
	Subject string   `json:"subject"`
	Preview string   `json:"preview"`
	Reason  string   `json:"reason"`
}

// Check is one doctor check of one account.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn"`
	Detail string `json:"detail"`
}

// Doctor is the plugin's report.
type Doctor struct {
	Accounts []struct {
		Sprig   string  `json:"sprig"`
		Address string  `json:"address"`
		Checks  []Check `json:"checks"`
	} `json:"accounts"`
}

// Client talks to the plugin's admin API.
type Client struct {
	URL   string // base, no trailing slash
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
}

// ErrNotFound is the plugin's 404: no such sprig, message or draft.
var ErrNotFound = errors.New("not found")

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	resp, err := c.raw(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (c *Client) raw(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
	if err != nil {
		return nil, err
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if resp.StatusCode == http.StatusNotFound {
		if e.Error != "" {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, e.Error)
		}
		return nil, ErrNotFound
	}
	if e.Error == "" {
		e.Error = resp.Status
	}
	return nil, fmt.Errorf("mail plugin: %s", e.Error)
}

// PutAccounts replaces the plugin's accounts.
func (c *Client) PutAccounts(ctx context.Context, accts []Account) ([]AccountState, error) {
	if accts == nil {
		accts = []Account{}
	}
	var out struct {
		Accounts []AccountState `json:"accounts"`
	}
	err := c.do(ctx, http.MethodPut, "/v1/accounts", map[string]any{"accounts": accts}, &out)
	return out.Accounts, err
}

// Events long-polls for events after seq, waiting up to wait.
func (c *Client) Events(ctx context.Context, after int64, wait time.Duration) (*Events, error) {
	var out Events
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(int(wait / time.Second))}}
	err := c.do(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil, &out)
	return &out, err
}

func box(sprig string) string { return "/v1/mailbox/" + url.PathEscape(sprig) }

// Mailbox lists a folder (inbox or drafts), newest first.
func (c *Client) Mailbox(ctx context.Context, sprig, folder string, limit int) ([]Summary, error) {
	var out struct {
		Messages []Summary `json:"messages"`
	}
	q := url.Values{"folder": {folder}, "limit": {strconv.Itoa(limit)}}
	err := c.do(ctx, http.MethodGet, box(sprig)+"?"+q.Encode(), nil, &out)
	return out.Messages, err
}

// Message reads one message as text.
func (c *Client) Message(ctx context.Context, sprig, id string) (*Message, error) {
	var out Message
	err := c.do(ctx, http.MethodGet, box(sprig)+"/messages/"+url.PathEscape(id), nil, &out)
	return &out, err
}

// Attachment opens one attachment's bytes; the caller closes it.
func (c *Client) Attachment(ctx context.Context, sprig, id, aid string) (io.ReadCloser, http.Header, error) {
	resp, err := c.raw(ctx, http.MethodGet, box(sprig)+"/messages/"+url.PathEscape(id)+"/attachments/"+url.PathEscape(aid), nil)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

// SendDraft sends a draft a person approved.
func (c *Client) SendDraft(ctx context.Context, sprig, id string) (string, error) {
	var out struct {
		MessageID string `json:"message_id"`
	}
	err := c.do(ctx, http.MethodPost, box(sprig)+"/drafts/"+url.PathEscape(id)+"/send", map[string]any{}, &out)
	return out.MessageID, err
}

// DiscardDraft deletes a draft.
func (c *Client) DiscardDraft(ctx context.Context, sprig, id string) error {
	return c.do(ctx, http.MethodPost, box(sprig)+"/drafts/"+url.PathEscape(id)+"/discard", map[string]any{}, nil)
}

// Doctor runs the plugin's own checks.
func (c *Client) Doctor(ctx context.Context) (*Doctor, error) {
	var out Doctor
	err := c.do(ctx, http.MethodGet, "/v1/doctor", nil, &out)
	return &out, err
}
