package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// The memory backend isn't safe for several connections at once (the
// plugin keeps two), so every call goes through one lock.

type lockedBackend struct {
	mu *sync.Mutex
	be backend.Backend
}

func (l lockedBackend) Login(ci *imap.ConnInfo, user, pass string) (backend.User, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, err := l.be.Login(ci, user, pass)
	if err != nil {
		return nil, err
	}
	return lockedUser{l.mu, u}, nil
}

type lockedUser struct {
	mu *sync.Mutex
	u  backend.User
}

func (l lockedUser) Username() string { return l.u.Username() }
func (l lockedUser) ListMailboxes(sub bool) ([]backend.Mailbox, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	list, err := l.u.ListMailboxes(sub)
	out := make([]backend.Mailbox, len(list))
	for i, m := range list {
		out[i] = lockedMailbox{l.mu, m}
	}
	return out, err
}
func (l lockedUser) GetMailbox(name string) (backend.Mailbox, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.u.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return lockedMailbox{l.mu, m}, nil
}
func (l lockedUser) CreateMailbox(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.u.CreateMailbox(name)
}
func (l lockedUser) DeleteMailbox(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.u.DeleteMailbox(name)
}
func (l lockedUser) RenameMailbox(a, b string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.u.RenameMailbox(a, b)
}
func (l lockedUser) Logout() error { return nil }

type lockedMailbox struct {
	mu *sync.Mutex
	m  backend.Mailbox
}

func (l lockedMailbox) Name() string { return l.m.Name() }
func (l lockedMailbox) Info() (*imap.MailboxInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Info()
}
func (l lockedMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Status(items)
}
func (l lockedMailbox) SetSubscribed(s bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.SetSubscribed(s)
}
func (l lockedMailbox) Check() error { return nil }
func (l lockedMailbox) ListMessages(uid bool, set *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	// Collect under the lock, hand over outside it.
	inner := make(chan *imap.Message)
	var got []*imap.Message
	done := make(chan struct{})
	go func() {
		for m := range inner {
			got = append(got, m)
		}
		close(done)
	}()
	l.mu.Lock()
	err := l.m.ListMessages(uid, set, items, inner)
	l.mu.Unlock()
	<-done
	for _, m := range got {
		ch <- m
	}
	close(ch)
	return err
}
func (l lockedMailbox) SearchMessages(uid bool, c *imap.SearchCriteria) ([]uint32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.SearchMessages(uid, c)
}
func (l lockedMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.CreateMessage(flags, date, body)
}
func (l lockedMailbox) UpdateMessagesFlags(uid bool, set *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.UpdateMessagesFlags(uid, set, op, flags)
}
func (l lockedMailbox) CopyMessages(uid bool, set *imap.SeqSet, dest string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.CopyMessages(uid, set, dest)
}
func (l lockedMailbox) Expunge() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Expunge()
}

// fakeIMAP is go-imap's memory server (user "username", password
// "password") with a fake SMTP server beside it.
type fakeIMAP struct {
	t       *testing.T
	be      lockedBackend
	imapLn  net.Listener
	smtpLn  net.Listener
	imapSec string
	smtpSec string
	address string

	mu   sync.Mutex
	sent []fakeSent
}

// newFakeIMAP starts both servers. imapSec/smtpSec "tls" serve TLS from
// the start, "starttls" offer STARTTLS, "none" offer no TLS at all.
func newFakeIMAP(t *testing.T, cert tls.Certificate, address, imapSec, smtpSec string) *fakeIMAP {
	f := &fakeIMAP{t: t, be: lockedBackend{&sync.Mutex{}, memory.New()}, imapSec: imapSec, smtpSec: smtpSec, address: address}
	conf := &tls.Config{Certificates: []tls.Certificate{cert}}
	quiet := log.New(io.Discard, "", 0)

	is := imapserver.New(f.be)
	is.ErrorLog = quiet
	f.imapLn = listen(t, imapSec, conf)
	if imapSec == "starttls" {
		is.TLSConfig = conf
	}
	if imapSec == "none" {
		is.AllowInsecureAuth = true
	}
	go func() { _ = is.Serve(f.imapLn) }()

	ss := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return &fakeSMTPSession{f: f}, nil }))
	ss.Domain = "localhost"
	ss.ErrorLog = quiet
	ss.AllowInsecureAuth = smtpSec == "none"
	if smtpSec == "starttls" {
		ss.TLSConfig = conf
	}
	f.smtpLn = listen(t, smtpSec, conf)
	go func() { _ = ss.Serve(f.smtpLn) }()
	t.Cleanup(func() {
		_ = is.Close()
		_ = ss.Close()
	})
	return f
}

func listen(t *testing.T, sec string, conf *tls.Config) net.Listener {
	var ln net.Listener
	var err error
	if sec == "tls" {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", conf)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func port(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

func (f *fakeIMAP) account() AccountConfig {
	sec := func(s string) string {
		if s == "none" {
			return "starttls" // the plugin asks for STARTTLS; the server doesn't offer it
		}
		return s
	}
	return AccountConfig{Sprig: "pip", Address: f.address, Name: "Pip", Backend: "imap",
		IMAP:     hostConfig{Host: "127.0.0.1", Port: port(f.imapLn), Security: sec(f.imapSec)},
		SMTP:     hostConfig{Host: "127.0.0.1", Port: port(f.smtpLn), Security: sec(f.smtpSec)},
		Username: "username", Password: "password"}
}

func (f *fakeIMAP) mailbox(name string) backend.Mailbox {
	u, err := f.be.Login(nil, "username", "password")
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := u.GetMailbox(name)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// deliver puts a message in INBOX as if it arrived.
func (f *fakeIMAP) deliver(from, subject, text, html string, at time.Time) {
	var h mail.Header
	h.SetDate(at)
	fa, _ := mail.ParseAddress(from)
	h.SetAddressList("From", []*mail.Address{fa})
	h.SetAddressList("To", []*mail.Address{{Address: f.address}})
	h.SetSubject(subject)
	h.SetMessageID(strconv.FormatInt(at.UnixNano(), 36) + "@vendor.example")
	var buf bytes.Buffer
	w, err := mail.CreateWriter(&buf, h)
	if err != nil {
		f.t.Fatal(err)
	}
	iw, _ := w.CreateInline()
	for _, p := range []struct{ ct, body string }{{"text/plain", text}, {"text/html", html}} {
		if p.body == "" {
			continue
		}
		var ih mail.InlineHeader
		ih.Set("Content-Type", p.ct+"; charset=utf-8")
		pw, _ := iw.CreatePart(ih)
		_, _ = io.WriteString(pw, p.body)
		_ = pw.Close()
	}
	_ = iw.Close()
	var ah mail.AttachmentHeader
	ah.Set("Content-Type", "application/pdf")
	ah.SetFilename("terms.pdf")
	aw, _ := w.CreateAttachment(ah)
	_, _ = io.WriteString(aw, "%PDF-1.4 fake")
	_ = aw.Close()
	_ = w.Close()
	if err := f.mailbox("INBOX").CreateMessage(nil, at, bytes.NewBuffer(buf.Bytes())); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeIMAP) sentMail() []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

type fakeSMTPSession struct {
	f     *fakeIMAP
	authd bool
	rcpts []string
}

func (s *fakeSMTPSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *fakeSMTPSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != "username" || pass != "password" {
			return errors.New("bad login")
		}
		s.authd = true
		return nil
	}), nil
}
func (s *fakeSMTPSession) Mail(string, *smtp.MailOptions) error {
	if !s.authd {
		return smtp.ErrAuthRequired
	}
	return nil
}
func (s *fakeSMTPSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}
func (s *fakeSMTPSession) Data(r io.Reader) error {
	raw, _ := io.ReadAll(r)
	m, _, err := parseRaw(raw, "")
	if err != nil {
		return err
	}
	if bytes.Contains(raw, []byte("\nBcc:")) {
		return errors.New("a Bcc header went over the wire")
	}
	sent := fakeSent{rcpts: s.rcpts, subject: m.Subject, body: m.Text}
	if len(m.InReplyTo) > 0 {
		sent.inReply = m.InReplyTo[0]
	}
	s.f.mu.Lock()
	s.f.sent = append(s.f.sent, sent)
	s.f.mu.Unlock()
	return nil
}
func (s *fakeSMTPSession) Reset()        { s.rcpts = nil }
func (s *fakeSMTPSession) Logout() error { return nil }
