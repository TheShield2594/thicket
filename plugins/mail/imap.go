package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// The IMAP/SMTP backend. IMAP is a stateful line protocol with literals,
// IDLE and MIME, so it uses go-imap's client rather than a hand-written
// one (see README.md). One connection serves the tools, one more waits
// in IDLE for new mail. Message ids are "<folder>-<uid>", e.g. inbox-42.

type imapBackend struct {
	cfg    AccountConfig
	tlsFor func(string) *tls.Config
	timing timing
	onNew  func(*Message)

	mu    sync.Mutex // the tools' connection and the folder names
	c     *client.Client
	names map[string]string // role -> mailbox name

	wmu      sync.Mutex // what the watcher has seen
	validity uint32
	next     uint32 // first UID not yet reported; 0 before the first connect
	push     string
}

func newIMAP(cfg AccountConfig, tlsFor func(string) *tls.Config, t timing, onNew func(*Message)) *imapBackend {
	return &imapBackend{cfg: cfg, tlsFor: tlsFor, timing: t, onNew: onNew}
}

// dial connects with TLS from the start ("tls") or after STARTTLS
// ("starttls"), and logs in. Plaintext is never used: a server that
// doesn't offer STARTTLS is refused before the password is sent.
func (b *imapBackend) dial() (*client.Client, error) {
	h := b.cfg.IMAP
	port := h.Port
	if port == 0 {
		port = map[string]int{"tls": 993, "starttls": 143}[h.Security]
	}
	addr := net.JoinHostPort(h.Host, strconv.Itoa(port))
	d := &net.Dialer{Timeout: 20 * time.Second}
	var c *client.Client
	var err error
	switch h.Security {
	case "tls":
		c, err = client.DialWithDialerTLS(d, addr, b.tlsFor(h.Host))
	case "starttls":
		c, err = client.DialWithDialer(d, addr)
		if err == nil {
			if ok, _ := c.SupportStartTLS(); !ok {
				c.Terminate()
				return nil, fmt.Errorf("IMAP server %s doesn't offer STARTTLS: refusing to log in without TLS", addr)
			}
			if err = c.StartTLS(b.tlsFor(h.Host)); err != nil {
				c.Terminate()
				return nil, fmt.Errorf("IMAP %s STARTTLS: %w", addr, err)
			}
		}
	default:
		return nil, fmt.Errorf("imap security %q refused: use tls or starttls (never plaintext)", h.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("IMAP %s: %w", addr, err)
	}
	c.Timeout = 2 * time.Minute
	c.ErrorLog = quietLog // errors reach us through the calls; don't log them twice
	if err := c.Login(b.cfg.Username, b.cfg.Password); err != nil {
		c.Terminate()
		return nil, errors.New("IMAP login failed: the server refused the username or password")
	}
	return c, nil
}

var quietLog = log.New(io.Discard, "", 0)

func alive(c *client.Client) bool {
	if c == nil {
		return false
	}
	select {
	case <-c.LoggedOut():
		return false
	default:
		return c.State() != imap.LogoutState
	}
}

// with runs fn on the tools' connection, dialing it again if it dropped.
func (b *imapBackend) with(fn func(c *client.Client) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !alive(b.c) {
		c, err := b.dial()
		if err != nil {
			return err
		}
		b.c = c
		if err := b.resolveNames(c); err != nil {
			return err
		}
	}
	err := fn(b.c)
	if err != nil && !alive(b.c) {
		b.c = nil
	}
	return err
}

var fallbackNames = map[string][]string{
	"drafts":  {"Drafts", "INBOX.Drafts", "Draft"},
	"sent":    {"Sent", "Sent Items", "Sent Messages", "INBOX.Sent"},
	"archive": {"Archive", "Archives", "INBOX.Archive"},
}

// resolveNames finds Drafts, Sent and Archive: by SPECIAL-USE attribute,
// else by the usual names (call with mu held).
func (b *imapBackend) resolveNames(c *client.Client) error {
	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- c.List("", "*", ch) }()
	names := map[string]string{"inbox": "INBOX"}
	var all []string
	for info := range ch {
		all = append(all, info.Name)
		for _, at := range info.Attributes {
			switch strings.ToLower(at) {
			case `\drafts`:
				names["drafts"] = info.Name
			case `\sent`:
				names["sent"] = info.Name
			case `\archive`:
				names["archive"] = info.Name
			}
		}
	}
	if err := <-done; err != nil {
		return err
	}
	for role, candidates := range fallbackNames {
		if names[role] != "" {
			continue
		}
		for _, cand := range candidates {
			if i := slices.IndexFunc(all, func(n string) bool { return strings.EqualFold(n, cand) }); i >= 0 {
				names[role] = all[i]
				break
			}
		}
	}
	b.names = names
	return nil
}

// box is the mailbox for a role, created when create is set and it
// doesn't exist yet (call inside with).
func (b *imapBackend) box(c *client.Client, role string, create bool) (string, error) {
	if n := b.names[role]; n != "" || !create {
		return n, nil
	}
	name := fallbackNames[role][0]
	if err := c.Create(name); err != nil {
		return "", fmt.Errorf("creating the %s folder: %w", name, err)
	}
	b.names[role] = name
	return name, nil
}

func (b *imapBackend) Connect(ctx context.Context) error {
	return b.with(func(c *client.Client) error {
		if err := c.Noop(); err != nil {
			return err
		}
		b.wmu.Lock()
		defer b.wmu.Unlock()
		if b.next != 0 {
			return nil
		}
		// Whatever is in the inbox now is not new mail.
		st, err := c.Status("INBOX", []imap.StatusItem{imap.StatusUidNext, imap.StatusUidValidity})
		if err != nil {
			return err
		}
		b.validity, b.next = st.UidValidity, max(st.UidNext, 1)
		return nil
	})
}

func parseID(id string) (string, uint32, error) {
	role, num, ok := strings.Cut(id, "-")
	uid, err := strconv.ParseUint(num, 10, 32)
	if !ok || err != nil || uid == 0 || !folders[role] {
		return "", 0, errNotFound
	}
	return role, uint32(uid), nil
}

func uidSet(uids ...uint32) *imap.SeqSet {
	s := new(imap.SeqSet)
	s.AddNum(uids...)
	return s
}

func fetch(c *client.Client, set *imap.SeqSet, items []imap.FetchItem) ([]*imap.Message, error) {
	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(set, items, ch) }()
	var out []*imap.Message
	for m := range ch {
		out = append(out, m)
	}
	return out, <-done
}

var (
	wholeBody  = &imap.BodySectionName{Peek: true}
	headerOnly = &imap.BodySectionName{BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier}, Peek: true}
)

// previewLimit: bigger messages are listed from their headers only.
const previewLimit = 256 << 10

// toMessage reads a fetched message (with whole body or header).
func (b *imapBackend) toMessage(role string, fm *imap.Message, section *imap.BodySectionName) *Message {
	var raw []byte
	if lit := fm.GetBody(section); lit != nil {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(lit)
		raw = buf.Bytes()
	}
	m, _, err := parseRaw(raw, "")
	if err != nil {
		m = &Message{Text: "[this message couldn't be read]"}
	}
	m.ID = role + "-" + strconv.FormatUint(uint64(fm.Uid), 10)
	m.Folder = role
	if !fm.InternalDate.IsZero() {
		m.Date = fm.InternalDate
	}
	m.Size = int64(fm.Size)
	m.Unread = !slices.Contains(fm.Flags, imap.SeenFlag)
	m.Draft = slices.Contains(fm.Flags, imap.DraftFlag) || role == "drafts"
	if section == headerOnly && fm.BodyStructure != nil {
		n := 0
		fm.BodyStructure.Walk(func(path []int, part *imap.BodyStructure) bool {
			if part.Disposition == "attachment" || (part.MIMEType != "text" && part.MIMEType != "multipart") {
				n++
			}
			return true
		})
		m.Attachments = n
	}
	return m
}

func (b *imapBackend) List(ctx context.Context, folder string, limit int, unreadOnly bool, since time.Time) ([]Summary, error) {
	var out []Summary
	err := b.with(func(c *client.Client) error {
		name, _ := b.box(c, folder, false)
		if name == "" {
			return nil
		}
		if _, err := c.Select(name, true); err != nil {
			return err
		}
		crit := imap.NewSearchCriteria()
		if unreadOnly {
			crit.WithoutFlags = []string{imap.SeenFlag}
		}
		if !since.IsZero() {
			crit.Since = since
		}
		uids, err := c.UidSearch(crit)
		if err != nil || len(uids) == 0 {
			return err
		}
		slices.Sort(uids)
		slices.Reverse(uids)
		uids = uids[:min(len(uids), limit)]
		meta, err := fetch(c, uidSet(uids...), []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size})
		if err != nil {
			return err
		}
		var small, big []uint32
		for _, m := range meta {
			if m.Size <= previewLimit {
				small = append(small, m.Uid)
			} else {
				big = append(big, m.Uid)
			}
		}
		base := []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size}
		got := map[uint32]*Message{}
		for _, part := range []struct {
			uids    []uint32
			section *imap.BodySectionName
			extra   []imap.FetchItem
		}{{small, wholeBody, nil}, {big, headerOnly, []imap.FetchItem{imap.FetchBodyStructure}}} {
			if len(part.uids) == 0 {
				continue
			}
			items := append(append(append([]imap.FetchItem{}, base...), part.extra...), part.section.FetchItem())
			msgs, err := fetch(c, uidSet(part.uids...), items)
			if err != nil {
				return err
			}
			for _, fm := range msgs {
				got[fm.Uid] = b.toMessage(folder, fm, part.section)
			}
		}
		for _, uid := range uids {
			if m := got[uid]; m != nil && (since.IsZero() || !m.Date.Before(since)) {
				out = append(out, m.Summary)
			}
		}
		return nil
	})
	return out, err
}

// raw fetches one message whole (call inside with).
func (b *imapBackend) raw(c *client.Client, id string, readOnly bool) (*imap.Message, string, error) {
	role, uid, err := parseID(id)
	if err != nil {
		return nil, "", err
	}
	name, _ := b.box(c, role, false)
	if name == "" {
		return nil, "", errNotFound
	}
	if _, err := c.Select(name, readOnly); err != nil {
		return nil, "", err
	}
	msgs, err := fetch(c, uidSet(uid), []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, wholeBody.FetchItem()})
	if err != nil {
		return nil, "", err
	}
	for _, m := range msgs {
		if m.Uid == uid {
			return m, role, nil
		}
	}
	return nil, "", errNotFound
}

func (b *imapBackend) Get(ctx context.Context, id string) (*Message, error) {
	var m *Message
	err := b.with(func(c *client.Client) error {
		fm, role, err := b.raw(c, id, true)
		if err != nil {
			return err
		}
		m = b.toMessage(role, fm, wholeBody)
		return nil
	})
	return m, err
}

func (b *imapBackend) MarkSeen(ctx context.Context, id string) error {
	return b.with(func(c *client.Client) error {
		role, uid, err := parseID(id)
		if err != nil {
			return err
		}
		name, _ := b.box(c, role, false)
		if name == "" {
			return errNotFound
		}
		if _, err := c.Select(name, false); err != nil {
			return err
		}
		return c.UidStore(uidSet(uid), imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.SeenFlag}, nil)
	})
}

func (b *imapBackend) Attachment(ctx context.Context, id, aid string) (Attachment, []byte, error) {
	var att Attachment
	var data []byte
	err := b.with(func(c *client.Client) error {
		fm, _, err := b.raw(c, id, true)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if lit := fm.GetBody(wholeBody); lit != nil {
			_, _ = buf.ReadFrom(lit)
		}
		m, d, err := parseRaw(buf.Bytes(), aid)
		if err != nil {
			return err
		}
		for _, a := range m.AttachmentList {
			if a.ID == aid {
				att, data = a, d
				return nil
			}
		}
		return errNotFound
	})
	return att, data, err
}

func (b *imapBackend) SaveDraft(ctx context.Context, out *Outgoing) (string, error) {
	now := time.Now()
	raw, err := composeRaw(out, true, now)
	if err != nil {
		return "", err
	}
	var id string
	err = b.with(func(c *client.Client) error {
		name, err := b.box(c, "drafts", true)
		if err != nil {
			return err
		}
		if err := c.Append(name, []string{imap.DraftFlag, imap.SeenFlag}, now, bytes.NewBuffer(raw)); err != nil {
			return err
		}
		// APPEND doesn't say the new UID without UIDPLUS: find it by its
		// Message-ID.
		if _, err := c.Select(name, true); err != nil {
			return err
		}
		crit := imap.NewSearchCriteria()
		crit.Header = textproto.MIMEHeader{"Message-Id": {"<" + out.MessageID + ">"}}
		uids, err := c.UidSearch(crit)
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			return errors.New("the draft was saved but can't be found in Drafts")
		}
		id = "drafts-" + strconv.FormatUint(uint64(slices.Max(uids)), 10)
		return nil
	})
	return id, err
}

func (b *imapBackend) Send(ctx context.Context, out *Outgoing) error {
	now := time.Now()
	wire, err := composeRaw(out, false, now) // no Bcc header on the wire
	if err != nil {
		return err
	}
	if err := smtpSend(ctx, b.cfg, b.tlsFor, b.cfg.Address, emails(out.recipients()), wire); err != nil {
		return err
	}
	copyRaw, err := composeRaw(out, true, now)
	if err == nil {
		err = b.with(func(c *client.Client) error {
			name, err := b.box(c, "sent", true)
			if err != nil {
				return err
			}
			return c.Append(name, []string{imap.SeenFlag}, now, bytes.NewBuffer(copyRaw))
		})
	}
	if err != nil {
		// It went out; only the copy is missing. Say so in the log.
		log.Printf("mail: %s: sent, but keeping a copy in Sent failed: %s", b.cfg.Sprig, b.cfg.scrub(err))
	}
	return nil
}

func (b *imapBackend) SendDraft(ctx context.Context, id string, m *Message) error {
	out := &Outgoing{
		From: Address{Name: cleanName(b.cfg.Name), Email: b.cfg.Address},
		To:   m.To, Cc: m.Cc, Bcc: m.Bcc, Subject: m.Subject, Body: m.Text,
		InReplyTo: firstOr(m.InReplyTo), References: m.References, MessageID: m.MessageID,
	}
	if out.MessageID == "" {
		out.MessageID = newMessageID(b.cfg.Address)
	}
	if err := b.Send(ctx, out); err != nil {
		return err
	}
	if err := b.Discard(ctx, id); err != nil {
		log.Printf("mail: %s: sent a draft but couldn't remove it from Drafts: %s", b.cfg.Sprig, b.cfg.scrub(err))
	}
	return nil
}

// remove flags a message \Deleted and expunges (call inside with, with
// its mailbox selected read-write). Without UIDPLUS, EXPUNGE also removes
// other messages already flagged \Deleted in that mailbox.
func remove(c *client.Client, uid uint32) error {
	if err := c.UidStore(uidSet(uid), imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	return c.Expunge(nil)
}

func (b *imapBackend) Discard(ctx context.Context, id string) error {
	role, uid, err := parseID(id)
	if err != nil {
		return err
	}
	if role != "drafts" {
		return errNotDraft
	}
	return b.with(func(c *client.Client) error {
		if _, _, err := b.raw(c, id, false); err != nil {
			return err
		}
		return remove(c, uid)
	})
}

func (b *imapBackend) Archive(ctx context.Context, id string) error {
	role, uid, err := parseID(id)
	if err != nil {
		return err
	}
	if role == "archive" {
		return nil
	}
	return b.with(func(c *client.Client) error {
		dest, err := b.box(c, "archive", true)
		if err != nil {
			return err
		}
		if _, _, err := b.raw(c, id, false); err != nil {
			return err
		}
		if err := c.UidMove(uidSet(uid), dest); err == nil {
			return nil
		}
		// MOVE unsupported or refused: COPY, then \Deleted and EXPUNGE.
		if err := c.UidCopy(uidSet(uid), dest); err != nil {
			return err
		}
		return remove(c, uid)
	})
}

// Watch keeps a second connection on the inbox: IDLE where the server
// has it (restarted every idleRefresh), polling otherwise.
func (b *imapBackend) Watch(ctx context.Context) error {
	c, err := b.dial()
	if err != nil {
		return err
	}
	defer c.Terminate()
	updates := make(chan client.Update, 64)
	c.Updates = updates
	wake := make(chan struct{}, 1)
	stopDrain := make(chan struct{})
	defer close(stopDrain)
	go func() {
		for {
			select {
			case u := <-updates:
				if _, ok := u.(*client.MailboxUpdate); ok {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			case <-stopDrain:
				return
			}
		}
	}()
	st, err := c.Select("INBOX", true)
	if err != nil {
		return err
	}
	b.wmu.Lock()
	if b.validity != st.UidValidity {
		// New UIDs for everything: start counting from now.
		b.validity, b.next = st.UidValidity, max(st.UidNext, 1)
	}
	b.wmu.Unlock()
	idle, _ := c.Support("IDLE")
	if idle {
		b.setPush("IDLE")
	} else {
		b.setPush(fmt.Sprintf("polling every %s", b.timing.poll))
	}
	for {
		if err := b.checkNew(c); err != nil {
			return err
		}
		if idle {
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- c.Idle(stop, &client.IdleOptions{LogoutTimeout: -1}) }()
			select {
			case <-wake:
			case <-time.After(b.timing.idleRefresh):
			case <-ctx.Done():
			case err := <-done:
				return fmt.Errorf("IDLE: %w", errOr(err))
			}
			close(stop)
			if err := <-done; err != nil && ctx.Err() == nil {
				return fmt.Errorf("IDLE: %w", err)
			}
		} else {
			select {
			case <-time.After(b.timing.poll):
			case <-ctx.Done():
			}
			if ctx.Err() == nil {
				if err := c.Noop(); err != nil {
					return err
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func errOr(err error) error {
	if err == nil {
		return errors.New("ended")
	}
	return err
}

func (b *imapBackend) setPush(s string) {
	b.wmu.Lock()
	b.push = s
	b.wmu.Unlock()
}

// checkNew reports inbox messages with a UID not seen yet.
func (b *imapBackend) checkNew(c *client.Client) error {
	b.wmu.Lock()
	next := b.next
	b.wmu.Unlock()
	crit := imap.NewSearchCriteria()
	crit.Uid = new(imap.SeqSet)
	crit.Uid.AddRange(next, 0) // next:* (also matches the last message when nothing is new)
	uids, err := c.UidSearch(crit)
	if err != nil {
		return err
	}
	uids = slices.DeleteFunc(uids, func(u uint32) bool { return u < next })
	if len(uids) == 0 {
		return nil
	}
	slices.Sort(uids)
	msgs, err := fetch(c, uidSet(uids...), []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, wholeBody.FetchItem()})
	if err != nil {
		return err
	}
	slices.SortFunc(msgs, func(a, b *imap.Message) int { return int(a.Uid) - int(b.Uid) })
	for _, fm := range msgs {
		if !slices.Contains(fm.Flags, imap.DraftFlag) {
			b.onNew(b.toMessage("inbox", fm, wholeBody))
		}
	}
	b.wmu.Lock()
	b.next = slices.Max(uids) + 1
	b.wmu.Unlock()
	return nil
}

func (b *imapBackend) Checks(ctx context.Context) []Check {
	h := b.cfg.IMAP
	c, err := b.dial()
	if err != nil {
		name := "session"
		if strings.Contains(err.Error(), "login failed") {
			name = "auth"
		}
		return []Check{{Name: name, OK: false, Detail: err.Error()}}
	}
	c.Terminate()
	checks := []Check{
		{Name: "session", OK: true, Detail: fmt.Sprintf("IMAP at %s:%d (%s)", h.Host, h.Port, h.Security)},
		{Name: "auth", OK: true},
	}
	b.wmu.Lock()
	push := b.push
	b.wmu.Unlock()
	if push == "" {
		push = "starting"
	}
	checks = append(checks, Check{Name: "push", OK: true, Detail: push})
	if sc, err := smtpDial(ctx, b.cfg, b.tlsFor); err != nil {
		checks = append(checks, Check{Name: "smtp", OK: false, Detail: err.Error()})
	} else {
		_ = sc.Quit()
		checks = append(checks, Check{Name: "smtp", OK: true, Detail: fmt.Sprintf("SMTP at %s:%d (%s)", b.cfg.SMTP.Host, b.cfg.SMTP.Port, b.cfg.SMTP.Security)})
	}
	return checks
}

func (b *imapBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		_ = b.c.Terminate()
		b.c = nil
	}
}
