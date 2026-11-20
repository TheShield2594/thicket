package main

import (
	"crypto/x509"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// mailFake is a fake provider: JMAP, or IMAP with SMTP.
type mailFake interface {
	account() AccountConfig
	deliver(from, subject, text, html string, at time.Time)
	sentMail() []fakeSent
}

const signupHTML = `<html><head><title>t</title><style>p{color:red}</style><script>alert("x")</script></head>
<body><p>Welcome to Vendor!</p>
<p><a href="  https://vendor.example/confirm?t=abc  ">Confirm your account</a></p>
<img src="https://track.example/pixel.gif" alt="Vendor logo"></body></html>`

// TestScenario is the acceptance scenario, against every backend.
func TestScenario(t *testing.T) {
	cert, pool := testCert(t)
	cases := []struct {
		name string
		push string // what the doctor should report
		make func(t *testing.T) mailFake
	}{
		{"jmap-eventsource", "EventSource", func(t *testing.T) mailFake { return newFakeJMAP(t, cert, "pip@example.org", true) }},
		{"jmap-polling", "polling every 100ms", func(t *testing.T) mailFake { return newFakeJMAP(t, cert, "pip@example.org", false) }},
		{"imap-tls-smtp-starttls", "IDLE", func(t *testing.T) mailFake { return newFakeIMAP(t, cert, "pip@example.org", "tls", "starttls") }},
		{"imap-starttls-smtp-tls", "IDLE", func(t *testing.T) mailFake { return newFakeIMAP(t, cert, "pip@example.org", "starttls", "tls") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScenario(t, tc.make(t), pool, tc.push) })
	}
}

func runScenario(t *testing.T, f mailFake, pool *x509.CertPool, push string) {
	h := newHarness(t, pool)

	// Mail already there when the account connects: no event, and this
	// one is older than the retention window.
	f.deliver("Old News <news@old.example>", "Old newsletter", "old stuff", "", time.Now().Add(-200*24*time.Hour))
	cfg := f.account()
	cfg.RetentionDays = 90
	cfg.SendLimit = 4
	if st := h.putAccounts(cfg); len(st) != 1 || st[0].State != "connected" {
		t.Fatalf("account: %+v", st)
	}
	me := callMeta{sprig: "pip"}

	// 1. A signup confirmation arrives.
	f.deliver("Vendor <hello@vendor.example>", "Confirm your signup", "", signupHTML, time.Now())
	ev := h.waitEvent("received")
	if ev.Subject != "Confirm your signup" || ev.From != "hello@vendor.example" || ev.OTP == nil || *ev.OTP || ev.Sprig != "pip" {
		t.Fatalf("received event: %+v", ev)
	}
	for _, e := range h.seen {
		if e.Kind == "received" && e.Subject != "Confirm your signup" {
			t.Fatalf("mail present at connect became an event: %+v", e)
		}
	}
	inbox := h.list("pip", "inbox")
	signup := findSubject(inbox, "Confirm your signup")
	if signup == nil || findSubject(inbox, "Old newsletter") != nil {
		t.Fatalf("inbox (retention applies): %+v", inbox)
	}
	r := h.mustTool("read_mail", map[string]any{"id": signup.ID}, me)
	var read struct {
		Text        string `json:"text"`
		From        string `json:"from"`
		Subject     string `json:"subject"`
		Attachments []Attachment
	}
	_ = json.Unmarshal([]byte(r.text()), &read)
	for _, want := range []string{"Welcome to Vendor!", "Confirm your account <https://vendor.example/confirm?t=abc>", "[image: Vendor logo]"} {
		if !strings.Contains(read.Text, want) {
			t.Errorf("read_mail text lacks %q:\n%s", want, read.Text)
		}
	}
	for _, bad := range []string{"alert", "color:red", "track.example", "<p>", "<script"} {
		if strings.Contains(read.Text, bad) {
			t.Errorf("read_mail text has %q:\n%s", bad, read.Text)
		}
	}
	if read.From != "Vendor <hello@vendor.example>" {
		t.Errorf("from: %q", read.From)
	}

	// The dashboard sees old mail; the sprig can't read it.
	_, b := h.do("GET", "/v1/mailbox/pip?folder=inbox", nil)
	var box struct {
		Messages []struct {
			ID, Subject string
		} `json:"messages"`
	}
	_ = json.Unmarshal(b, &box)
	oldID := ""
	for _, m := range box.Messages {
		if m.Subject == "Old newsletter" {
			oldID = m.ID
		}
	}
	if oldID == "" {
		t.Fatalf("dashboard doesn't show old mail: %s", b)
	}
	if r := h.tool("read_mail", map[string]any{"id": oldID}, me); !r.IsError || !strings.Contains(r.text(), "window") {
		t.Fatalf("old mail readable by the sprig: %+v", r)
	}

	// 2. Replying without approval to someone not on the list: drafted.
	r = h.mustTool("reply_mail", map[string]any{"id": signup.ID, "body": "Thanks, confirmed!"}, me)
	d := r.drafted()
	if d == nil || d.DraftID == "" || !slices.Equal(d.To, []string{"hello@vendor.example"}) ||
		d.Reason != "hello@vendor.example isn't on Pip's mail allowlist" || d.Subject != "Re: Confirm your signup" ||
		d.Preview != "Thanks, confirmed!" || d.From != "pip@example.org" {
		t.Fatalf("reply drafted: %+v %q", d, r.text())
	}
	if e := h.waitEvent("drafted"); e.DraftID != d.DraftID || e.Reason != d.Reason {
		t.Fatalf("drafted event: %+v", e)
	}
	if n := len(f.sentMail()); n != 0 {
		t.Fatalf("sent %d", n)
	}
	_, b = h.do("GET", "/v1/mailbox/pip/messages/"+d.DraftID, nil)
	if !strings.Contains(string(b), `"draft":true`) || !strings.Contains(string(b), "Thanks, confirmed!") {
		t.Fatalf("draft in the dashboard: %s", b)
	}

	// 3. send_draft without approval: still not sent, nothing new drafted.
	r = h.mustTool("send_draft", map[string]any{"draft_id": d.DraftID}, me)
	if r.drafted() != nil || !strings.Contains(r.text(), "needs you") || len(f.sentMail()) != 0 {
		t.Fatalf("unapproved send_draft: %+v", r)
	}
	// With approval (the person's "Do it"): sent.
	r = h.mustTool("send_draft", map[string]any{"draft_id": d.DraftID}, callMeta{sprig: "pip", approved: true})
	if r.drafted() != nil || !strings.HasPrefix(r.text(), "Sent") {
		t.Fatalf("approved send_draft: %+v", r)
	}
	sent := f.sentMail()
	if len(sent) != 1 || !slices.Equal(sent[0].rcpts, []string{"hello@vendor.example"}) || sent[0].subject != "Re: Confirm your signup" || sent[0].inReply == "" {
		t.Fatalf("sent: %+v", sent)
	}
	if e := h.waitEvent("sent"); e.Approved == nil || !*e.Approved || e.Subject != "Re: Confirm your signup" {
		t.Fatalf("sent event: %+v", e)
	}
	if findSubject(h.list("pip", "drafts"), "Re: Confirm") != nil || findSubject(h.list("pip", "sent"), "Re: Confirm your signup") == nil {
		t.Fatalf("after sending: drafts %+v sent %+v", h.list("pip", "drafts"), h.list("pip", "sent"))
	}

	// 4. Replying didn't put the vendor on any list: a second reply drafts.
	if d := h.mustTool("reply_mail", map[string]any{"id": signup.ID, "body": "again"}, me).drafted(); d == nil {
		t.Fatal("second reply was sent")
	}

	// 5. send_mail to allowlisted addresses: sent directly.
	allow := []string{"shop@vendor.example", "@partner.example"}
	al := callMeta{sprig: "pip", allow: allow}
	msg := func(to []string, cc []string) map[string]any {
		return map[string]any{"to": to, "cc": cc, "subject": "Order", "body": "One please."}
	}
	for _, to := range []string{"Shop@Vendor.Example", "sales@partner.example"} {
		r = h.mustTool("send_mail", msg([]string{to}, nil), al)
		if r.drafted() != nil || !strings.HasPrefix(r.text(), "Sent") {
			t.Fatalf("send to %s: %+v", to, r)
		}
	}
	if n := len(f.sentMail()); n != 3 {
		t.Fatalf("sent %d", n)
	}
	// @domain doesn't cover subdomains.
	if d := h.mustTool("send_mail", msg([]string{"x@sub.partner.example"}, nil), al).drafted(); d == nil || !strings.Contains(d.Reason, "x@sub.partner.example") {
		t.Fatalf("subdomain: %+v", d)
	}
	// One cc off the list: drafted, naming it.
	d = h.mustTool("send_mail", msg([]string{"shop@vendor.example"}, []string{"jane@x.example"}), al).drafted()
	if d == nil || d.Reason != "jane@x.example isn't on Pip's mail allowlist" || !slices.Equal(d.Cc, []string{"jane@x.example"}) {
		t.Fatalf("cc: %+v", d)
	}
	// Draft level: drafted even though allowlisted.
	d = h.mustTool("send_mail", msg([]string{"shop@vendor.example"}, nil), callMeta{sprig: "pip", allow: allow, draftOnly: true}).drafted()
	if d == nil || !strings.Contains(d.Reason, "draft level") {
		t.Fatalf("draft_only: %+v", d)
	}
	// draft_mail always drafts.
	if d := h.mustTool("draft_mail", msg([]string{"shop@vendor.example"}, nil), al).drafted(); d == nil {
		t.Fatal("draft_mail sent")
	}

	// 6. The daily limit (4): one more send, then drafts.
	if r := h.mustTool("send_mail", msg([]string{"shop@vendor.example"}, nil), al); r.drafted() != nil {
		t.Fatalf("4th send: %+v", r)
	}
	d = h.mustTool("send_mail", msg([]string{"shop@vendor.example"}, nil), al).drafted()
	if d == nil || !strings.Contains(d.Reason, "daily send limit") {
		t.Fatalf("limit: %+v", d)
	}
	// Approved sends count but are never refused.
	if r := h.mustTool("send_mail", msg([]string{"jane@x.example"}, nil), callMeta{sprig: "pip", approved: true}); r.drafted() != nil {
		t.Fatalf("approved past the limit: %+v", r)
	}
	if n := len(f.sentMail()); n != 5 {
		t.Fatalf("sent %d", n)
	}

	// 7. A one-time code (before archiving: go-imap's memory backend reuses
	// the UID of an expunged last message, which real servers never do): hidden from the sprig, shown to the person.
	f.deliver("Bank <no-reply@bank.example>", "Your verification code", "Your verification code is 482913. It expires in 10 minutes.", "", time.Now())
	if e := h.waitEvent("received"); e.OTP == nil || !*e.OTP {
		t.Fatalf("otp event: %+v", e)
	}
	otp := findSubject(h.list("pip", "inbox"), "verification code")
	if otp == nil || strings.Contains(otp.Preview, "482913") {
		t.Fatalf("otp in listing: %+v", otp)
	}
	r = h.mustTool("read_mail", map[string]any{"id": otp.ID}, me)
	if strings.Contains(r.text(), "482913") || !strings.Contains(r.text(), "one-time code hidden") {
		t.Fatalf("otp read: %s", r.text())
	}
	_, b = h.do("GET", "/v1/mailbox/pip/messages/"+otp.ID, nil)
	if !strings.Contains(string(b), "482913") {
		t.Fatalf("person can't see the code: %s", b)
	}

	// 8. Discard a draft (the person dismissed it) and archive the signup.
	code, b := h.do("POST", "/v1/mailbox/pip/drafts/"+d.DraftID+"/discard", nil)
	if code != 200 {
		t.Fatalf("discard: %d %s", code, b)
	}
	if e := h.waitEvent("discarded"); e.DraftID != d.DraftID {
		t.Fatalf("discarded: %+v", e)
	}
	h.mustTool("archive_mail", map[string]any{"id": signup.ID}, me)
	h.waitEvent("archived")
	if findSubject(h.list("pip", "archive"), "Confirm your signup") == nil || findSubject(h.list("pip", "inbox"), "Confirm your signup") != nil {
		t.Fatal("archive didn't move the message")
	}

	// 9. A person sends a draft from the dashboard.
	d = h.mustTool("draft_mail", msg([]string{"zed@else.example"}, nil), me).drafted()
	code, b = h.do("POST", "/v1/mailbox/pip/drafts/"+d.DraftID+"/send", nil)
	if code != 200 || !strings.Contains(string(b), "message_id") {
		t.Fatalf("dashboard send: %d %s", code, b)
	}
	if e := h.waitEvent("sent"); !slices.Equal(e.To, []string{"zed@else.example"}) || !*e.Approved {
		t.Fatalf("dashboard sent event: %+v", e)
	}

	// 10. Doctor.
	_, b = h.do("GET", "/v1/doctor", nil)
	for _, want := range []string{`"name":"session","ok":true`, `"name":"push","ok":true,"detail":"` + push + `"`, `"name":"smtp","ok":true`, `"name":"dns"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("doctor lacks %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), cfg.Password) {
		t.Fatal("doctor shows the password")
	}
}
