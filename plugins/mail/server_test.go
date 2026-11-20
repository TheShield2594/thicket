package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuth(t *testing.T) {
	h := newHarness(t, nil)
	paths := []struct{ method, path, body string }{
		{"POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"PUT", "/v1/accounts", `{"accounts":[]}`},
		{"GET", "/v1/events", ""},
		{"GET", "/v1/doctor", ""},
		{"GET", "/v1/mailbox/pip", ""},
		{"POST", "/v1/mailbox/pip/drafts/x/send", ""},
		{"GET", "/v1/nothing", ""},
	}
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + testToken[:31], "Basic " + testToken, testToken} {
		for _, p := range paths {
			req, _ := http.NewRequest(p.method, h.srv.URL+p.path, strings.NewReader(p.body))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s %s with %q: %d", p.method, p.path, auth, resp.StatusCode)
			}
		}
	}
	if resp, err := http.Get(h.srv.URL + "/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v", err)
	}
	if code, b := h.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); code != 200 || !strings.Contains(string(b), `"thicket/gates_itself":true`) {
		t.Fatalf("tools/list: %d %s", code, b)
	}
	// A token file with a short token refuses everything, even that token.
	os.WriteFile(h.s.tokenFile, []byte("short"), 0o600)
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer short")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("short token: %d", resp.StatusCode)
	}
}

func TestToolsList(t *testing.T) {
	read, gated, trailed := 0, 0, 0
	for _, tl := range toolList() {
		m := tl.(map[string]any)
		if m["annotations"].(map[string]any)["readOnlyHint"] == true {
			read++
		}
		if meta, ok := m["_meta"].(map[string]any); ok {
			if meta["thicket/gates_itself"] == true {
				gated++
			}
			if meta["thicket/trailed"] == true {
				trailed++
			}
		}
	}
	if read != 2 || gated != 4 || trailed != 6 {
		t.Fatalf("read %d gated %d trailed %d", read, gated, trailed)
	}
}

func TestRefusesPlaintext(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)

	// Security values other than tls/starttls are refused outright.
	f := newFakeIMAP(t, cert, "pip@example.org", "tls", "tls")
	for _, sec := range []string{"none", "plain", ""} {
		cfg := f.account()
		cfg.IMAP.Security = sec
		st := h.putAccounts(cfg)
		if st[0].State != "error" || !strings.Contains(st[0].Error, "never plaintext") {
			t.Fatalf("imap %q: %+v", sec, st)
		}
		cfg = f.account()
		cfg.SMTP.Security = sec
		if st := h.putAccounts(cfg); st[0].State != "error" || !strings.Contains(st[0].Error, "never plaintext") {
			t.Fatalf("smtp %q: %+v", sec, st)
		}
	}
	// A JMAP session URL must be https.
	if st := h.putAccounts(AccountConfig{Sprig: "pip", Address: "pip@example.org", Backend: "jmap", JMAP: jmapConfig{SessionURL: "http://x/"}}); st[0].State != "error" {
		t.Fatalf("http jmap: %+v", st)
	}

	// An IMAP server that offers no STARTTLS: refused before LOGIN.
	plain := newFakeIMAP(t, cert, "pip@example.org", "none", "none")
	st := h.putAccounts(plain.account())
	if st[0].State != "error" || !strings.Contains(st[0].Error, "IMAP server") || !strings.Contains(st[0].Error, "doesn't offer STARTTLS") {
		t.Fatalf("imap without STARTTLS: %+v", st)
	}
	// An SMTP server that offers no STARTTLS: refused before AUTH.
	cfg := plain.account()
	_, err := smtpDial(context.Background(), cfg, h.s.tlsConfig)
	if err == nil || !strings.Contains(err.Error(), "SMTP server") || !strings.Contains(err.Error(), "doesn't offer STARTTLS") {
		t.Fatalf("smtp without STARTTLS: %v", err)
	}
	if err := smtpSend(context.Background(), cfg, h.s.tlsConfig, cfg.Address, []string{"a@x.example"}, []byte("x")); err == nil {
		t.Fatal("sent without TLS")
	}
	if n := len(plain.sentMail()); n != 0 {
		t.Fatalf("sent %d", n)
	}

	// A certificate nobody vouches for is refused (production verifies).
	h.s.rootCAs = nil
	if st := h.putAccounts(f.account()); st[0].State != "error" || !strings.Contains(st[0].Error, "certificate") {
		t.Fatalf("untrusted certificate: %+v", st)
	}
	j := newFakeJMAP(t, cert, "pip@example.org", true)
	if st := h.putAccounts(j.account()); st[0].State != "error" || !strings.Contains(st[0].Error, "certificate") {
		t.Fatalf("untrusted JMAP certificate: %+v", st)
	}
}

func TestAccountsLifecycle(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	j := newFakeJMAP(t, cert, "pip@example.org", true)
	cfg := j.account()
	if st := h.putAccounts(cfg); st[0].State != "connected" {
		t.Fatalf("%+v", st)
	}
	first := h.s.lookup("pip")
	h.putAccounts(cfg) // unchanged: same connection
	if h.s.lookup("pip") != first {
		t.Fatal("unchanged account reconnected")
	}
	// A wrong password: error, and the password isn't in it.
	cfg.Password = "wrong-password-123"
	st := h.putAccounts(cfg)
	if st[0].State != "error" || !strings.Contains(st[0].Error, "login failed") || strings.Contains(st[0].Error, cfg.Password) {
		t.Fatalf("%+v", st)
	}
	if e := h.waitEvent("error"); strings.Contains(e.Detail, cfg.Password) || e.Detail == "" {
		t.Fatalf("error event: %+v", e)
	}
	// Removed: tools say there's no mailbox; the admin API says 404.
	h.putAccounts()
	if r := h.tool("list_mail", map[string]any{}, callMeta{sprig: "pip"}); !r.IsError || r.text() != "Pip has no mail address yet; its owner can set one up" {
		t.Fatalf("%+v", r)
	}
	if r := h.tool("list_mail", map[string]any{}, callMeta{}); !r.IsError || !strings.Contains(r.text(), "no mail address") {
		t.Fatalf("%+v", r)
	}
	// 503, not 404: core must not take "no account (yet)" for "draft gone".
	if code, _ := h.do("GET", "/v1/mailbox/pip", nil); code != 503 {
		t.Fatalf("admin: %d", code)
	}
}

func TestCaps(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	j := newFakeJMAP(t, cert, "pip@example.org", false)
	h.putAccounts(j.account())
	me := callMeta{sprig: "pip", approved: true}

	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf("a%d@x.example", i)
	}
	for name, args := range map[string]map[string]any{
		"21 recipients": {"to": many, "subject": "s", "body": "b"},
		"long subject":  {"to": []string{"a@x.example"}, "subject": strings.Repeat("s", 301), "body": "b"},
		"long body":     {"to": []string{"a@x.example"}, "subject": "s", "body": strings.Repeat("b", 64<<10+1)},
		"bad address":   {"to": []string{"not an address"}, "subject": "s", "body": "b"},
		"header inject": {"to": []string{"a@x.example\r\nBcc: b@x.example"}, "subject": "s", "body": "b"},
		"nobody":        {"to": []string{}, "subject": "s", "body": "b"},
	} {
		if r := h.tool("send_mail", args, me); !r.IsError {
			t.Errorf("%s: accepted: %s", name, r.text())
		}
	}
	if n := len(j.sentMail()); n != 0 {
		t.Fatalf("sent %d", n)
	}

	// Received: body cut at 32 KiB, at most 20 attachments listed.
	e := &jEmail{From: []jAddr{{Email: "big@x.example"}}, Subject: "Big", TextBody: []jPart{{PartID: "1", Type: "text/plain"}}}
	e.BodyValues = map[string]struct {
		Value       string `json:"value"`
		IsTruncated bool   `json:"isTruncated"`
	}{"1": {Value: strings.Repeat("é", 40<<10)}}
	for i := range 25 {
		e.Attach = append(e.Attach, jPart{BlobID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("f%d.pdf", i), Type: "application/pdf", Size: 10})
	}
	id := j.add("mb-inbox", e)
	r := h.mustTool("read_mail", map[string]any{"id": id}, me)
	var out struct {
		Text        string `json:"text"`
		Attachments []any  `json:"attachments"`
		More        int    `json:"more_attachments"`
	}
	_ = json.Unmarshal([]byte(r.text()), &out)
	if len(out.Text) > 32<<10+100 || !strings.Contains(out.Text, "[cut: the message is longer than 32 KiB]") || len(out.Attachments) != 20 || out.More != 5 {
		t.Fatalf("caps: len %d attachments %d more %d", len(out.Text), len(out.Attachments), out.More)
	}
	// The person can download an attachment.
	code, b := h.do("GET", "/v1/mailbox/pip/messages/"+id+"/attachments/b3", nil)
	if code != 200 || string(b) != "blob b3" {
		t.Fatalf("attachment: %d %s", code, b)
	}
}

func TestSendLimitPersists(t *testing.T) {
	dir := t.TempDir()
	l, _ := openSendLog(dir)
	now := time.Now()
	for i := range 3 {
		if _, err := l.reserve("pip", 3, false, now.Add(-time.Duration(i)*time.Hour)); err != nil {
			t.Fatal("refused under the limit")
		}
	}
	if _, err := l.reserve("pip", 3, false, now); !errors.Is(err, errLimit) {
		t.Fatal("over the limit")
	}
	if _, err := l.reserve("pip", 3, true, now); err != nil {
		t.Fatal("approved send refused")
	}
	// After a restart the count is still there; old sends age out.
	l2, err := openSendLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := l2.count("pip", now); n != 4 {
		t.Fatalf("count after restart %d", n)
	}
	if n := l2.count("pip", now.Add(sendWindow-30*time.Minute)); n != 2 {
		t.Fatalf("rolling window: %d", n)
	}
	// A failed send gives its slot back.
	release, _ := l2.reserve("wren", 1, false, now)
	release()
	if n := l2.count("wren", now); n != 0 {
		t.Fatalf("released: %d", n)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "sends.json" {
		t.Fatalf("state dir: %v", entries)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "sends.json"))
	if !json.Valid(b) {
		t.Fatalf("state: %s", b)
	}
}

func TestEvents(t *testing.T) {
	l := newEventLog(3)
	for i := range 5 {
		l.add(Event{Sprig: "pip", Kind: "sent", Subject: fmt.Sprint(i)})
	}
	got, next, _ := l.after(0)
	if len(got) != 3 || got[0].Seq != 3 || next != 5 {
		t.Fatalf("%+v %d", got, next)
	}
	// Long poll: wakes on a new event.
	h := newHarness(t, nil)
	h.poll(0)
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.s.events.add(Event{Sprig: "pip", Kind: "received"})
	}()
	start := time.Now()
	if ev := h.poll(5); len(ev) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("long poll: %+v", ev)
	}
	// Nothing new: answers empty after the wait.
	_, b := h.do("GET", "/v1/events?after=1&wait=1", nil)
	if !strings.Contains(string(b), `"events":[]`) || !strings.Contains(string(b), h.s.events.epoch) {
		t.Fatalf("%s", b)
	}
}

func TestDoctorDNS(t *testing.T) {
	s := &server{resolver: fakeResolver{
		"example.org":                    {"v=spf1 include:spf.example.net ~all"},
		"fm1._domainkey.example.org":     {"v=DKIM1; k=rsa; p=MIIB"},
		"_dmarc.good.example":            {"v=DMARC1; p=none"},
		"good.example":                   {"v=spf1 -all"},
		"google._domainkey.good.example": {"v=DKIM1; p=abc"},
	}}
	c := s.dnsCheck(context.Background(), "pip@example.org")
	if !c.OK || !c.Warn || c.Detail != "no DMARC record for example.org" {
		t.Fatalf("%+v", c)
	}
	c = s.dnsCheck(context.Background(), "pip@good.example")
	if !c.OK || c.Warn || !strings.Contains(c.Detail, "DKIM (google)") {
		t.Fatalf("%+v", c)
	}
	c = s.dnsCheck(context.Background(), "pip@none.example")
	if !c.Warn || !strings.Contains(c.Detail, "no SPF") || !strings.Contains(c.Detail, "no DKIM") {
		t.Fatalf("%+v", c)
	}
}
