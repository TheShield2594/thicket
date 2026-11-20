package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// AccountConfig is one account as core pushes it (PUT /v1/accounts). It
// lives in memory only: the password is never written to disk or logged.
type AccountConfig struct {
	Sprig         string     `json:"sprig"`
	Address       string     `json:"address"`
	Name          string     `json:"name"`
	Backend       string     `json:"backend"`
	JMAP          jmapConfig `json:"jmap"`
	IMAP          hostConfig `json:"imap"`
	SMTP          hostConfig `json:"smtp"`
	Username      string     `json:"username"`
	Password      string     `json:"password"`
	RetentionDays int        `json:"retention_days"`
	SendLimit     int        `json:"send_limit"`
}

type jmapConfig struct {
	SessionURL string `json:"session_url"`
}

type hostConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
}

// Defaults when core sends no send_limit / retention_days (or 0), the
// same as docs/mail.md.
const (
	defaultSendLimit = 20
	defaultRetention = 90
)

func (c AccountConfig) retentionDays() int {
	if c.RetentionDays <= 0 {
		return defaultRetention
	}
	return c.RetentionDays
}

func (c AccountConfig) sendLimit() int {
	if c.SendLimit <= 0 {
		return defaultSendLimit
	}
	return c.SendLimit
}

// displayName is how the sprig is named in messages: "Pip".
func (c AccountConfig) displayName() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return cleanLine(n)
	}
	return titleName(c.Sprig)
}

func titleName(sprig string) string {
	r := []rune(cleanLine(sprig))
	if len(r) == 0 {
		return "This sprig"
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// validate refuses a config that can't work, before anything connects.
func (c AccountConfig) validate() error {
	if !strings.Contains(c.Address, "@") {
		return errors.New("address is missing or not an email address")
	}
	switch c.Backend {
	case "jmap":
		if !strings.HasPrefix(c.JMAP.SessionURL, "https://") {
			return errors.New("jmap.session_url must be an https:// URL")
		}
	case "imap":
		if c.IMAP.Host == "" || c.SMTP.Host == "" {
			return errors.New("imap and smtp hosts are required")
		}
		if c.IMAP.Security != "tls" && c.IMAP.Security != "starttls" {
			return fmt.Errorf("imap security %q refused: use tls or starttls (never plaintext)", c.IMAP.Security)
		}
		if c.SMTP.Security != "tls" && c.SMTP.Security != "starttls" {
			return fmt.Errorf("smtp security %q refused: use tls or starttls (never plaintext)", c.SMTP.Security)
		}
	default:
		return fmt.Errorf("backend %q unknown: use jmap or imap", c.Backend)
	}
	return nil
}

// Address is one mailbox: a display name (may be empty) and an address.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// String writes the address for the sprig or the dashboard. The name is
// already cleaned of anything that could pass for another address.
func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

// Summary is one message in a listing.
type Summary struct {
	ID          string
	Folder      string // inbox, drafts, sent, archive or other
	From        Address
	To          []Address
	Subject     string
	Date        time.Time
	Size        int64
	Preview     string
	Attachments int
	Unread      bool
	Draft       bool
}

// Attachment describes one attachment (never its content).
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// Message is one message read in full, its body already turned into text.
type Message struct {
	Summary
	Cc, Bcc, ReplyTo []Address
	MessageID        string // without the angle brackets
	InReplyTo        []string
	References       []string
	Text             string
	AttachmentList   []Attachment
}

// Outgoing is a message the plugin composes.
type Outgoing struct {
	From       Address
	To, Cc     []Address
	Bcc        []Address
	Subject    string
	Body       string
	InReplyTo  string // bare message id
	References []string
	MessageID  string // bare; set by compose
}

func (o *Outgoing) recipients() []Address {
	all := append(append(append([]Address{}, o.To...), o.Cc...), o.Bcc...)
	return all
}

// Check is one line of /v1/doctor.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Backend is one connected mailbox. JMAP and IMAP/SMTP implement it.
// Message ids are the backend's own and opaque to everyone else.
type Backend interface {
	// Connect logs in and learns the mailbox layout. The first successful
	// connect also notes what is already in the inbox: that is never an
	// event.
	Connect(ctx context.Context) error
	// Watch waits for new mail in the inbox and hands each message to the
	// onNew function the backend was made with. It returns when ctx ends
	// or the connection fails.
	Watch(ctx context.Context) error
	List(ctx context.Context, folder string, limit int, unreadOnly bool, since time.Time) ([]Summary, error)
	Get(ctx context.Context, id string) (*Message, error)
	MarkSeen(ctx context.Context, id string) error
	Attachment(ctx context.Context, id, aid string) (Attachment, []byte, error)
	SaveDraft(ctx context.Context, out *Outgoing) (string, error)
	// Send submits a new message and keeps a copy in Sent.
	Send(ctx context.Context, out *Outgoing) error
	// SendDraft submits a draft already in Drafts (msg is that draft, read
	// with Get) and moves it to Sent.
	SendDraft(ctx context.Context, id string, msg *Message) error
	Discard(ctx context.Context, id string) error
	Archive(ctx context.Context, id string) error
	Checks(ctx context.Context) []Check
	Close()
}

// account is one configured account and its connection loop.
type account struct {
	cfg    AccountConfig
	key    string // the whole config as JSON: unchanged key, keep the connection
	be     Backend
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	state   string // connecting, connected, error
	err     string
	settled chan struct{} // closed after the first connect attempt
	once    sync.Once
}

func (a *account) status() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.err
}

func (a *account) set(state, err string) {
	a.mu.Lock()
	a.state, a.err = state, err
	a.mu.Unlock()
	a.once.Do(func() { close(a.settled) })
}

// scrub removes the account's password (plain and in a Basic auth header)
// from an error before anyone sees it.
func (c AccountConfig) scrub(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if c.Password != "" {
		msg = strings.ReplaceAll(msg, c.Password, "[password]")
		basic := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
		msg = strings.ReplaceAll(msg, basic, "[password]")
	}
	return msg
}

func (s *server) newBackend(cfg AccountConfig, onNew func(*Message)) Backend {
	if cfg.Backend == "jmap" {
		return newJMAP(cfg, s.tlsConfig, s.timing, onNew)
	}
	return newIMAP(cfg, s.tlsConfig, s.timing, onNew)
}

// startAccount connects an account in the background, reconnecting with
// growing delays after errors.
func (s *server) startAccount(cfg AccountConfig, key string) *account {
	ctx, cancel := context.WithCancel(context.Background())
	a := &account{cfg: cfg, key: key, cancel: cancel, done: make(chan struct{}), state: "connecting", settled: make(chan struct{})}
	a.be = s.newBackend(cfg, func(m *Message) { s.received(a, m) })
	go func() {
		defer close(a.done)
		defer a.be.Close()
		delay := s.timing.retryMin
		lastErr := ""
		for {
			err := a.be.Connect(ctx)
			if err == nil {
				a.set("connected", "")
				lastErr = ""
				delay = s.timing.retryMin
				err = a.be.Watch(ctx)
			}
			if ctx.Err() != nil {
				return
			}
			msg := cfg.scrub(err)
			a.set("error", msg)
			if msg != lastErr { // one event per distinct error, not one per retry
				log.Printf("mail: %s: %s", cfg.Sprig, msg)
				s.events.add(Event{Sprig: cfg.Sprig, Kind: "error", Detail: msg})
				lastErr = msg
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, s.timing.retryMax)
		}
	}()
	return a
}

func (a *account) stop() {
	a.cancel()
	<-a.done
}

// received turns new inbox mail into a `received` event.
func (s *server) received(a *account, m *Message) {
	_, otp := maskOTP(m.Subject, m.Text)
	s.events.add(Event{
		Sprig: a.cfg.Sprig, Kind: "received",
		From: m.From.Email, To: emails(m.To), Cc: emails(m.Cc), Subject: m.Subject,
		Size: m.Size, MessageID: bracket(m.MessageID), OTP: &otp,
	})
}

func emails(l []Address) []string {
	out := make([]string, 0, len(l))
	for _, a := range l {
		out = append(out, a.Email)
	}
	return out
}

func bracket(id string) string {
	if id == "" {
		return ""
	}
	return "<" + id + ">"
}

// lookup finds the account for a sprig.
func (s *server) lookup(sprig string) *account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[sprig]
}

// ready returns the account's backend if it is connected.
func (a *account) ready() (Backend, error) {
	state, msg := a.status()
	if state != "connected" {
		if msg == "" {
			msg = "still connecting"
		}
		return nil, fmt.Errorf("%s's mail account isn't connected right now (%s)", a.cfg.displayName(), msg)
	}
	return a.be, nil
}

type accountStatus struct {
	Sprig string `json:"sprig"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// putAccounts replaces the whole set of accounts. An account whose config
// didn't change keeps its connection.
func (s *server) putAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Accounts []AccountConfig `json:"accounts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request body")
		return
	}
	type entry struct {
		cfg AccountConfig
		bad error
	}
	want := map[string]entry{}
	order := []string{}
	for _, c := range body.Accounts {
		c.Sprig = strings.TrimSpace(c.Sprig)
		c.Address = strings.ToLower(strings.TrimSpace(c.Address))
		if c.Sprig == "" {
			jsonError(w, http.StatusBadRequest, "an account has no sprig")
			return
		}
		if _, dup := want[c.Sprig]; dup {
			jsonError(w, http.StatusBadRequest, "two accounts for sprig "+c.Sprig)
			return
		}
		want[c.Sprig] = entry{cfg: c, bad: c.validate()}
		order = append(order, c.Sprig)
	}

	s.mu.Lock()
	var stopping []*account
	for sprig, a := range s.accounts {
		e, ok := want[sprig]
		if ok && e.bad == nil {
			if key, _ := json.Marshal(e.cfg); string(key) == a.key {
				continue
			}
		}
		stopping = append(stopping, a)
		delete(s.accounts, sprig)
	}
	var invalid = map[string]string{}
	for sprig, e := range want {
		if e.bad != nil {
			invalid[sprig] = e.bad.Error()
			continue
		}
		if _, ok := s.accounts[sprig]; ok {
			continue
		}
		key, _ := json.Marshal(e.cfg)
		s.accounts[sprig] = s.startAccount(e.cfg, string(key))
	}
	current := map[string]*account{}
	for k, v := range s.accounts {
		current[k] = v
	}
	s.mu.Unlock()
	for _, a := range stopping {
		go a.stop()
	}

	// Give new connections a moment so the answer is useful.
	deadline := time.After(s.timing.connectWait)
	for _, a := range current {
		select {
		case <-a.settled:
		case <-deadline:
		}
	}
	out := []accountStatus{}
	for _, sprig := range order {
		if msg, bad := invalid[sprig]; bad {
			out = append(out, accountStatus{Sprig: sprig, State: "error", Error: msg})
			continue
		}
		state, msg := current[sprig].status()
		out = append(out, accountStatus{Sprig: sprig, State: state, Error: msg})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
