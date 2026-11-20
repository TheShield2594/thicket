package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert is a self-signed certificate for 127.0.0.1, and a pool that
// trusts it: the fakes serve TLS with it, the plugin verifies it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "thicket-mail test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

// fakeResolver answers DNS TXT questions from a map.
type fakeResolver map[string][]string

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := f[name]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

var fastTiming = timing{
	poll:        100 * time.Millisecond,
	idleRefresh: 100 * time.Millisecond,
	retryMin:    50 * time.Millisecond,
	retryMax:    200 * time.Millisecond,
	connectWait: 5 * time.Second,
	pushFails:   2,
}

const testToken = "0123456789abcdef0123456789abcdef-test"

type harness struct {
	t     *testing.T
	s     *server
	srv   *httptest.Server
	after int64
	seen  []Event
}

func newHarness(t *testing.T, pool *x509.CertPool) *harness {
	t.Helper()
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(tokFile, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	s.rootCAs = pool
	s.timing = fastTiming
	s.resolver = fakeResolver{}
	h := &harness{t: t, s: s, srv: httptest.NewServer(s.handler())}
	t.Cleanup(func() {
		h.srv.Close()
		s.mu.Lock()
		accts := s.accounts
		s.accounts = map[string]*account{}
		s.mu.Unlock()
		for _, a := range accts {
			a.stop()
		}
	})
	return h
}

func (h *harness) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *harness) putAccounts(cfgs ...AccountConfig) []accountStatus {
	h.t.Helper()
	code, b := h.do("PUT", "/v1/accounts", map[string]any{"accounts": cfgs})
	if code != 200 {
		h.t.Fatalf("PUT /v1/accounts: %d %s", code, b)
	}
	var out struct {
		Accounts []accountStatus `json:"accounts"`
	}
	_ = json.Unmarshal(b, &out)
	return out.Accounts
}

// callMeta is what core puts in _meta.
type callMeta struct {
	sprig     string
	approved  bool
	draftOnly bool
	allow     []string
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Meta map[string]json.RawMessage `json:"_meta"`
}

func (r toolResult) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

type drafted struct {
	DraftID string   `json:"draft_id"`
	From    string   `json:"from"`
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Bcc     []string `json:"bcc"`
	Subject string   `json:"subject"`
	Preview string   `json:"preview"`
	Reason  string   `json:"reason"`
}

func (r toolResult) drafted() *drafted {
	raw, ok := r.Meta["thicket/drafted"]
	if !ok {
		return nil
	}
	var d drafted
	_ = json.Unmarshal(raw, &d)
	return &d
}

func (h *harness) tool(name string, args any, m callMeta) toolResult {
	h.t.Helper()
	allow := m.allow
	if allow == nil {
		allow = []string{}
	}
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{
			"thicket/call": map[string]any{"sprig": m.sprig, "approved": m.approved, "draft_only": m.draftOnly},
			"thicket/mail": map[string]any{"allow": allow},
		},
	}}
	code, b := h.do("POST", "/mcp", body)
	if code != 200 {
		h.t.Fatalf("%s: HTTP %d %s", name, code, b)
	}
	var resp struct {
		Result toolResult `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		h.t.Fatalf("%s: %v %s", name, err, b)
	}
	return resp.Result
}

// mustTool calls a tool and fails the test on a tool error.
func (h *harness) mustTool(name string, args any, m callMeta) toolResult {
	h.t.Helper()
	r := h.tool(name, args, m)
	if r.IsError {
		h.t.Fatalf("%s: tool error: %s", name, r.text())
	}
	return r
}

// poll reads new events (waiting up to wait seconds).
func (h *harness) poll(wait int) []Event {
	h.t.Helper()
	_, b := h.do("GET", "/v1/events?after="+itoa(h.after)+"&wait="+itoa(int64(wait)), nil)
	var out struct {
		Epoch  string  `json:"epoch"`
		Next   int64   `json:"next"`
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("events: %v %s", err, b)
	}
	h.after = out.Next
	h.seen = append(h.seen, out.Events...)
	return out.Events
}

// waitEvent waits for the next event of a kind and returns it; events of
// other kinds pass by.
func (h *harness) waitEvent(kind string) Event {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.poll(2) {
			if e.Kind == kind {
				return e
			}
		}
	}
	h.t.Fatalf("no %s event; saw %+v", kind, h.seen)
	return Event{}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// listIDs is list_mail's messages.
type listed struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Preview string `json:"preview"`
	Unread  bool   `json:"unread"`
}

func (h *harness) list(sprig, folder string) []listed {
	h.t.Helper()
	r := h.mustTool("list_mail", map[string]any{"folder": folder}, callMeta{sprig: sprig})
	var out struct {
		Messages []listed `json:"messages"`
	}
	if err := json.Unmarshal([]byte(r.text()), &out); err != nil {
		h.t.Fatalf("list_mail: %v %s", err, r.text())
	}
	return out.Messages
}

func findSubject(l []listed, sub string) *listed {
	for i := range l {
		if strings.Contains(l[i].Subject, sub) {
			return &l[i]
		}
	}
	return nil
}
