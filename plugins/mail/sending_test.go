package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-message/mail"
)

// Sending safety: nothing goes out twice, nothing goes out that wasn't
// approved, and an unconfirmed submission isn't retried.

// TestSMTPUnconfirmed: the server takes the message and hangs up before
// answering DATA. That is not a failure to retry: the send keeps its
// count, and the draft is never sent again.
func TestSMTPUnconfirmed(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	f := newFakeIMAP(t, cert, "pip@example.org", "tls", "tls")
	h.putAccounts(f.account())
	f.mu.Lock()
	f.dropAfterData = true
	f.mu.Unlock()
	al := callMeta{sprig: "pip", allow: []string{"shop@vendor.example"}}

	r := h.tool("send_mail", map[string]any{"to": []string{"shop@vendor.example"}, "subject": "s", "body": "b"}, al)
	if !r.IsError || !strings.Contains(r.text(), "may have been sent") {
		t.Fatalf("send_mail: %+v", r)
	}
	if n := h.s.sends.count("pip", time.Now()); n != 1 {
		t.Fatalf("an unconfirmed send must keep its count: %d", n)
	}

	d := h.mustTool("draft_mail", map[string]any{"to": []string{"zed@else.example"}, "subject": "d", "body": "b"}, al).drafted()
	ok := callMeta{sprig: "pip", approved: true}
	if r := h.tool("send_draft", map[string]any{"draft_id": d.DraftID}, ok); !r.IsError || !strings.Contains(r.text(), "may have been sent") {
		t.Fatalf("send_draft: %+v", r)
	}
	if n := len(f.sentMail()); n != 2 {
		t.Fatalf("sent %d", n)
	}
	// "Do it" again, or the dashboard: never a second submission.
	if r := h.tool("send_draft", map[string]any{"draft_id": d.DraftID}, ok); !r.IsError || !strings.Contains(r.text(), "already sent") {
		t.Fatalf("second send_draft: %+v", r)
	}
	if code, b := h.do("POST", "/v1/mailbox/pip/drafts/"+d.DraftID+"/send", nil); code != 404 {
		t.Fatalf("dashboard resend: %d %s", code, b)
	}
	if n := len(f.sentMail()); n != 2 {
		t.Fatalf("sent again: %d", n)
	}
}

// TestIMAPDraftSendsOriginal: a draft made in a mail client (HTML,
// attachment, Bcc) goes out as it is, Bcc in the envelope only.
func TestIMAPDraftSendsOriginal(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	f := newFakeIMAP(t, cert, "pip@example.org", "tls", "tls")
	u, _ := f.be.Login(nil, "username", "password")
	_ = u.CreateMailbox("Drafts")

	var hd mail.Header
	hd.SetAddressList("From", []*mail.Address{{Address: "pip@example.org"}})
	hd.SetAddressList("To", []*mail.Address{{Address: "ann@x.example"}})
	hd.SetAddressList("Bcc", []*mail.Address{{Address: "boss@y.example"}})
	hd.SetSubject("Report")
	var buf bytes.Buffer
	w, _ := mail.CreateWriter(&buf, hd)
	iw, _ := w.CreateInline()
	var ih mail.InlineHeader
	ih.Set("Content-Type", "text/html; charset=utf-8")
	pw, _ := iw.CreatePart(ih)
	_, _ = pw.Write([]byte("<p>See <b>attached</b></p>"))
	_ = pw.Close()
	_ = iw.Close()
	var ah mail.AttachmentHeader
	ah.Set("Content-Type", "application/pdf")
	ah.SetFilename("report.pdf")
	aw, _ := w.CreateAttachment(ah)
	_, _ = aw.Write([]byte("%PDF-1.4 report"))
	_ = aw.Close()
	_ = w.Close()
	if err := f.mailbox("Drafts").CreateMessage([]string{imap.DraftFlag}, time.Now(), bytes.NewBuffer(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	h.putAccounts(f.account())

	draft := findSubject(h.list("pip", "drafts"), "Report")
	if draft == nil {
		t.Fatal("no draft")
	}
	r := h.mustTool("send_draft", map[string]any{"draft_id": draft.ID}, callMeta{sprig: "pip", approved: true})
	sent := f.sentMail()
	if len(sent) != 1 {
		t.Fatalf("sent %d: %s", len(sent), r.text())
	}
	raw := string(sent[0].raw)
	for _, want := range []string{"report.pdf", "text/html"} {
		if !strings.Contains(raw, want) {
			t.Errorf("sent message lacks %q:\n%s", want, raw)
		}
	}
	if _, att, err := parseRaw(sent[0].raw, "1"); err != nil || string(att) != "%PDF-1.4 report" {
		t.Errorf("attachment: %q %v", att, err)
	}
	if strings.Contains(raw, "Bcc") || strings.Contains(raw, "boss@y.example") {
		t.Errorf("Bcc on the wire:\n%s", raw)
	}
	if !slices.Contains(sent[0].rcpts, "boss@y.example") || !slices.Contains(sent[0].rcpts, "ann@x.example") {
		t.Errorf("envelope: %v", sent[0].rcpts)
	}
	// The result names the Message-ID that went out (one was added).
	m, _, _ := parseRaw(sent[0].raw, "")
	if m.MessageID == "" || !strings.Contains(r.text(), "<"+m.MessageID+">") {
		t.Errorf("message id %q not in %q", m.MessageID, r.text())
	}
	if findSubject(h.list("pip", "drafts"), "Report") != nil || findSubject(h.list("pip", "sent"), "Report") == nil {
		t.Error("draft not moved to Sent")
	}
}

// TestIMAPRemoveOnlyOurs: discarding a draft without UIDPLUS doesn't
// expunge a message another client flagged \Deleted.
func TestIMAPRemoveOnlyOurs(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	f := newFakeIMAP(t, cert, "pip@example.org", "tls", "tls")
	u, _ := f.be.Login(nil, "username", "password")
	_ = u.CreateMailbox("Drafts")
	if err := f.mailbox("Drafts").CreateMessage([]string{imap.DeletedFlag}, time.Now(), bytes.NewBufferString("Subject: other\r\n\r\nx\r\n")); err != nil {
		t.Fatal(err)
	}
	h.putAccounts(f.account())
	me := callMeta{sprig: "pip"}
	d := h.mustTool("draft_mail", map[string]any{"to": []string{"a@x.example"}, "subject": "mine", "body": "b"}, me).drafted()
	h.mustTool("discard_draft", map[string]any{"draft_id": d.DraftID}, me)
	if findSubject(h.list("pip", "drafts"), "mine") != nil {
		t.Fatal("draft still listed")
	}
	st, _ := f.mailbox("Drafts").Status([]imap.StatusItem{imap.StatusMessages})
	if st.Messages != 2 {
		t.Fatalf("the other client's \\Deleted message was expunged: %d left", st.Messages)
	}
}

// TestIMAPIDs: ids carry UIDVALIDITY; an id for another one is not found.
func TestIMAPIDs(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	f := newFakeIMAP(t, cert, "pip@example.org", "tls", "tls")
	h.putAccounts(f.account())
	// Many at once: reported in batches, every one of them.
	for i := range 25 {
		f.deliver("A <a@x.example>", "hello "+itoa(int64(i)), "hi", "", time.Now())
	}
	got := 0
	deadline := time.Now().Add(10 * time.Second)
	for got < 25 && time.Now().Before(deadline) {
		for _, e := range h.poll(1) {
			if e.Kind == "received" {
				got++
			}
		}
	}
	if got != 25 {
		t.Fatalf("received events: %d", got)
	}
	l := h.list("pip", "inbox")
	role, v, uid, err := parseID(l[0].ID)
	if err != nil || role != "inbox" || v != 1 {
		t.Fatalf("id %q: %v", l[0].ID, err)
	}
	me := callMeta{sprig: "pip"}
	h.mustTool("read_mail", map[string]any{"id": l[0].ID}, me)
	for _, id := range []string{makeID("inbox", 2, uid), "inbox-" + itoa(int64(uid)), "inbox-x-1"} {
		if r := h.tool("read_mail", map[string]any{"id": id}, me); !r.IsError {
			t.Errorf("%s: %s", id, r.text())
		}
	}
	if code, _ := h.do("GET", "/v1/mailbox/pip/messages/"+makeID("inbox", 2, uid), nil); code != 404 {
		t.Errorf("stale id on the admin API: %d", code)
	}
}

// TestBcc: an allowlisted Bcc goes out in the envelope only; one off the
// list makes a draft.
func TestBcc(t *testing.T) {
	cert, pool := testCert(t)
	for name, mk := range map[string]func(t *testing.T) mailFake{
		"jmap": func(t *testing.T) mailFake { return newFakeJMAP(t, cert, "pip@example.org", false) },
		"imap": func(t *testing.T) mailFake { return newFakeIMAP(t, cert, "pip@example.org", "tls", "starttls") },
	} {
		t.Run(name, func(t *testing.T) {
			f := mk(t)
			h := newHarness(t, pool)
			h.putAccounts(f.account())
			al := callMeta{sprig: "pip", allow: []string{"shop@vendor.example", "@partner.example"}}
			r := h.mustTool("send_mail", map[string]any{"to": []string{"shop@vendor.example"}, "bcc": []string{"boss@partner.example"}, "subject": "s", "body": "b"}, al)
			if r.drafted() != nil {
				t.Fatalf("allowlisted bcc drafted: %+v", r)
			}
			sent := f.sentMail()
			if len(sent) != 1 || !slices.Equal(sent[0].rcpts, []string{"shop@vendor.example", "boss@partner.example"}) {
				t.Fatalf("sent: %+v", sent)
			}
			if sent[0].raw != nil && bytes.Contains(sent[0].raw, []byte("boss@partner.example")) {
				t.Fatalf("bcc in the message:\n%s", sent[0].raw)
			}
			d := h.mustTool("send_mail", map[string]any{"to": []string{"shop@vendor.example"}, "bcc": []string{"spy@else.example"}, "subject": "s", "body": "b"}, al).drafted()
			if d == nil || !strings.Contains(d.Reason, "spy@else.example") || !slices.Equal(d.Bcc, []string{"spy@else.example"}) {
				t.Fatalf("off-list bcc: %+v", d)
			}
			if n := len(f.sentMail()); n != 1 {
				t.Fatalf("sent %d", n)
			}
		})
	}
}

// TestReplyAndRetention: a broken Reply-To falls back to From, a code in
// the subject stays hidden in the reply, and an unapproved send_draft
// respects the retention window.
func TestReplyAndRetention(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	j := newFakeJMAP(t, cert, "pip@example.org", false)
	h.putAccounts(j.account())
	me := callMeta{sprig: "pip"}

	id := j.add("mb-inbox", &jEmail{From: []jAddr{{Email: "bank@bank.example"}}, ReplyTo: []jAddr{{Email: "not an address"}},
		Subject: "Your verification code is 739201"})
	d := h.mustTool("reply_mail", map[string]any{"id": id, "body": "thanks"}, me).drafted()
	if d == nil || !slices.Equal(d.To, []string{"bank@bank.example"}) || strings.Contains(d.Subject, "739201") || !strings.HasPrefix(d.Subject, "Re: ") {
		t.Fatalf("reply: %+v", d)
	}

	old := j.add("mb-drafts", &jEmail{To: []jAddr{{Email: "shop@vendor.example"}}, Subject: "old draft",
		Keywords: map[string]bool{"$draft": true}, ReceivedAt: time.Now().Add(-200 * 24 * time.Hour)})
	al := callMeta{sprig: "pip", allow: []string{"shop@vendor.example"}}
	if r := h.tool("send_draft", map[string]any{"draft_id": old}, al); !r.IsError || !strings.Contains(r.text(), "window") {
		t.Fatalf("old draft sent by the sprig: %+v", r)
	}
	if r := h.mustTool("send_draft", map[string]any{"draft_id": old}, callMeta{sprig: "pip", approved: true}); !strings.HasPrefix(r.text(), "Sent") {
		t.Fatalf("approved old draft: %+v", r)
	}
	if j.readUsedSubmission {
		t.Error("a read-only request asked for the submission capability")
	}
}

// TestJMAPWithoutSubmission: a read-only JMAP account still reads; sending
// says why it can't.
func TestJMAPWithoutSubmission(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	j := newFakeJMAP(t, cert, "pip@example.org", false)
	j.noSubmit = true
	if st := h.putAccounts(j.account()); st[0].State != "connected" {
		t.Fatalf("%+v", st)
	}
	j.deliver("A <a@x.example>", "hi", "hello", "", time.Now())
	if findSubject(h.list("pip", "inbox"), "hi") == nil {
		t.Fatal("can't read")
	}
	r := h.tool("send_mail", map[string]any{"to": []string{"a@x.example"}, "subject": "s", "body": "b"}, callMeta{sprig: "pip", approved: true})
	if !r.IsError || !strings.Contains(r.text(), "doesn't offer sending") {
		t.Fatalf("%+v", r)
	}
}

func TestJMAPAttachmentTooBig(t *testing.T) {
	cert, pool := testCert(t)
	h := newHarness(t, pool)
	j := newFakeJMAP(t, cert, "pip@example.org", false)
	h.putAccounts(j.account())
	id := j.add("mb-inbox", &jEmail{From: []jAddr{{Email: "a@x.example"}}, Subject: "s",
		Attach: []jPart{{BlobID: "b1", Name: "f.bin", Size: 7}}})
	defer func(n int) { maxAttachment = n }(maxAttachment)
	maxAttachment = len("blob b1") // exactly at the limit: fine
	if code, b := h.do("GET", "/v1/mailbox/pip/messages/"+id+"/attachments/b1", nil); code != 200 || string(b) != "blob b1" {
		t.Fatalf("at the limit: %d %s", code, b)
	}
	maxAttachment--
	if code, b := h.do("GET", "/v1/mailbox/pip/messages/"+id+"/attachments/b1", nil); code == 200 || !strings.Contains(string(b), "too big") {
		t.Fatalf("over the limit: %d %s", code, b)
	}
}

func TestDraftClaims(t *testing.T) {
	var d draftClaims
	now := time.Now()
	if err := d.claim("pip", "d1", now); err != nil {
		t.Fatal(err)
	}
	if err := d.claim("pip", "d1", now); !errors.Is(err, errDraftBusy) {
		t.Fatalf("busy: %v", err)
	}
	if err := d.claim("wren", "d1", now); err != nil {
		t.Fatalf("another sprig's d1: %v", err)
	}
	d.end("pip", "d1", false, now) // a definite failure: may try again
	if err := d.claim("pip", "d1", now); err != nil {
		t.Fatal(err)
	}
	d.end("pip", "d1", true, now) // sent: never again
	if err := d.claim("pip", "d1", now); !errors.Is(err, errDraftSent) {
		t.Fatalf("sent: %v", err)
	}
	if err := d.claim("pip", "d1", now.Add(keepSent+time.Hour)); err != nil {
		t.Fatalf("forgotten after keepSent: %v", err)
	}
}

func TestSendLogSaveFails(t *testing.T) {
	l, _ := openSendLog(t.TempDir())
	l.path = filepath.Join(t.TempDir(), "gone", "sends.json") // can't be written
	now := time.Now()
	if _, err := l.reserve("pip", 5, false, now); err == nil || errors.Is(err, errLimit) {
		t.Fatalf("unapproved send with no saved count: %v", err)
	}
	if n := l.count("pip", now); n != 0 {
		t.Fatalf("not rolled back: %d", n)
	}
	if _, err := l.reserve("pip", 5, true, now); err != nil {
		t.Fatalf("approved send refused: %v", err)
	}
}

func TestAdminUnknownSprig(t *testing.T) {
	h := newHarness(t, nil)
	for _, p := range []string{"/v1/mailbox/pip/drafts/d1/discard", "/v1/mailbox/pip/drafts/d1/send"} {
		if code, b := h.do("POST", p, nil); code != 503 || !strings.Contains(string(b), "isn't connected") {
			t.Errorf("%s: %d %s", p, code, b)
		}
	}
}

func TestMCPErrors(t *testing.T) {
	h := newHarness(t, nil)
	for body, want := range map[string]float64{
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`:                       -32601,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`:    -32602,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`:     -32602,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1, 2]}`: -32602,
	} {
		req, _ := http.NewRequest("POST", h.srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Error struct {
				Code float64 `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out.Error.Code != want {
			t.Errorf("%s: code %v", body, out.Error.Code)
		}
	}
}

// TestSlowBody: a client that never finishes its body is cut off.
func TestSlowBody(t *testing.T) {
	h := newHarness(t, nil)
	h.s.timing.bodyRead = 200 * time.Millisecond
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /mcp HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + testToken + "\r\nContent-Length: 100\r\n\r\n{"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 512)
	n, _ := conn.Read(b)
	if !strings.Contains(string(b[:n]), "408") {
		t.Fatalf("slow body: %q", b[:n])
	}
}

// TestConnectWaitShared: PUT /v1/accounts waits once for all accounts,
// not once for the first and forever for the rest.
func TestConnectWaitShared(t *testing.T) {
	hang := make(chan struct{})
	stall := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	cert, pool := testCert(t)
	stall.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	stall.StartTLS()
	defer stall.Close()
	defer close(hang)
	h := newHarness(t, pool)
	h.s.timing.connectWait = 300 * time.Millisecond
	acct := func(sprig string) AccountConfig {
		return AccountConfig{Sprig: sprig, Address: sprig + "@example.org", Backend: "jmap",
			JMAP: jmapConfig{SessionURL: stall.URL + "/session"}, Username: "u", Password: "p"}
	}
	start := time.Now()
	st := h.putAccounts(acct("pip"), acct("wren"), acct("moss"))
	if time.Since(start) > 3*time.Second || len(st) != 3 || st[0].State != "connecting" {
		t.Fatalf("took %s: %+v", time.Since(start), st)
	}
}
