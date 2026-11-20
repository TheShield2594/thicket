// Command thicket-mail is the mail plugin for Thicket: it gives each sprig
// a mailbox (JMAP, or IMAP with SMTP) through MCP tools, and gives Thicket
// core an admin API for accounts, events and the dashboard's mailbox view.
// PROTOCOL.md in this folder is the contract with core; docs/plugins.md
// has the rules every plugin follows.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// version is reported to Thicket (and `thicket doctor`) when it connects.
var version = "0.1.0"

func main() {
	addr := env("MAIL_ADDR", ":7072")
	s, err := newServer(env("MAIL_TOKEN_FILE", "/plugin-auth/token"), env("MAIL_STATE_DIR", "/var/lib/thicket-mail"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mail plugin %s listening on %s", version, addr)
	srv := &http.Server{Addr: addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// timing holds every interval the plugin waits on, so tests can shrink
// them. Production uses defaultTiming.
type timing struct {
	poll        time.Duration // polling fallback when there is no push
	idleRefresh time.Duration // restart IMAP IDLE this often (servers drop idle clients)
	retryMin    time.Duration // first reconnect delay after an error
	retryMax    time.Duration // longest reconnect delay
	connectWait time.Duration // how long PUT /v1/accounts waits for a first answer
	bodyRead    time.Duration // how long a client may take to send a request body
	pushFails   int           // failed EventSource attempts in a row before polling
}

var defaultTiming = timing{
	poll:        60 * time.Second,
	idleRefresh: 20 * time.Minute,
	retryMin:    5 * time.Second,
	retryMax:    5 * time.Minute,
	connectWait: 5 * time.Second,
	bodyRead:    30 * time.Second,
	pushFails:   3,
}

// txtResolver is the part of net.Resolver the doctor uses, so tests can
// answer DNS questions themselves.
type txtResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type server struct {
	tokenFile string
	events    *eventLog
	sends     *sendLog
	drafts    draftClaims // a draft is sent once (tools.go)
	resolver  txtResolver
	timing    timing
	// rootCAs is nil in production (the system's roots). Tests set it to
	// trust their self-signed fakes. Certificates are always verified.
	rootCAs *x509.CertPool
	now     func() time.Time

	mu       sync.Mutex
	accounts map[string]*account // by sprig
}

func newServer(tokenFile, stateDir string) (*server, error) {
	sends, err := openSendLog(stateDir)
	if err != nil {
		return nil, err
	}
	return &server{
		tokenFile: tokenFile,
		events:    newEventLog(1000),
		sends:     sends,
		resolver:  net.DefaultResolver,
		timing:    defaultTiming,
		now:       time.Now,
		accounts:  map[string]*account{},
	}, nil
}

// tlsConfig is the one TLS configuration for every outbound connection:
// certificates verified against the system roots (or the test's), TLS 1.2
// at least.
func (s *server) tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, RootCAs: s.rootCAs, MinVersion: tls.VersionTLS12}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	// /health answers without a token: Thicket uses it to see whether the
	// plugin is running at all.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /mcp", s.guard(s.mcp))
	mux.HandleFunc("PUT /v1/accounts", s.guard(s.putAccounts))
	mux.HandleFunc("GET /v1/events", s.guard(s.getEvents))
	mux.HandleFunc("GET /v1/doctor", s.guard(s.doctor))
	mux.HandleFunc("GET /v1/mailbox/{sprig}", s.guard(s.mailboxList))
	mux.HandleFunc("GET /v1/mailbox/{sprig}/messages/{id}", s.guard(s.mailboxMessage))
	mux.HandleFunc("GET /v1/mailbox/{sprig}/messages/{id}/attachments/{aid}", s.guard(s.mailboxAttachment))
	mux.HandleFunc("POST /v1/mailbox/{sprig}/drafts/{id}/send", s.guard(s.mailboxSend))
	mux.HandleFunc("POST /v1/mailbox/{sprig}/drafts/{id}/discard", s.guard(s.mailboxDiscard))
	// Anything else under /v1/ still needs the token before it learns
	// that the path doesn't exist.
	mux.HandleFunc("/v1/", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w, http.StatusNotFound, "not found")
	}))
	return mux
}

// maxBody is the biggest request body any route takes.
const maxBody = 1 << 20

// guard refuses a request without the right token, and reads its body
// (if any) under a deadline, so a client that sends it slowly can't hold
// a connection. The deadline is per request, not the server's
// ReadTimeout: that one would also cut the long poll on /v1/events and
// long sends short. After the body, the deadline is lifted.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(s.timing.bodyRead))
			b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil {
				http.Error(w, "request body too slow or broken", http.StatusRequestTimeout)
				return
			}
			if len(b) > maxBody {
				http.Error(w, "request body too big", http.StatusRequestEntityTooLarge)
				return
			}
			_ = rc.SetReadDeadline(time.Time{})
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h(w, r)
	}
}

// authorized checks the bearer token against the shared file, read on
// every request so a new token takes effect without a restart. With no
// token file (or a short token) nothing is allowed.
func (s *server) authorized(r *http.Request) bool {
	b, err := os.ReadFile(s.tokenFile)
	want := strings.TrimSpace(string(b))
	if err != nil || len(want) < 32 {
		return false
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(h, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
