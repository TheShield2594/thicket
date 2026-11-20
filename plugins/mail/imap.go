package main

import (
	"bufio"
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
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	gmtextproto "github.com/emersion/go-message/textproto"
)

// The IMAP/SMTP backend. IMAP is a stateful line protocol with literals,
// IDLE and MIME, so it uses go-imap's client rather than a hand-written
// one (see README.md). One connection serves the tools, one more waits
// in IDLE for new mail. Message ids are "<folder>-<uidvalidity>-<uid>",
// e.g. inbox-1700000000-42: a UID means something only together with the
// folder's UIDVALIDITY, so an id from before the server renumbered a folder
// is "not found", never another message.

type imapBackend struct {
	cfg    AccountConfig
	tlsFor func(string) *tls.Config
	timing timing
	onNew  func(*Message)

	mu      sync.Mutex // the tools' connection, the folder names, cleanup
	c       *client.Client
	names   map[string]string // role -> mailbox name
	cleanup map[string]bool   // ids of drafts sent but not yet removed from Drafts

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
		if err == nil {
			c.Timeout = 2 * time.Minute
		}
	case "starttls":
		c, err = client.DialWithDialer(d, addr)
		if err == nil {
			// Every command from here on, CAPABILITY and STARTTLS included,
			// has a deadline (the greeting had the dialer's).
			c.Timeout = 2 * time.Minute
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
		b.c = nil
		c, err := b.dial()
		if err != nil {
			return err
		}
		// Only a connection that knows the folders is kept: otherwise the
		// next call would use it without names.
		if err := b.resolveNames(c); err != nil {
			c.Terminate()
			return err
		}
		b.c = c
	}
	if len(b.cleanup) > 0 {
		b.tidyDrafts(b.c)
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

func parseID(id string) (role string, validity, uid uint32, err error) {
	parts := strings.Split(id, "-")
	if len(parts) != 3 || !folders[parts[0]] {
		return "", 0, 0, errNotFound
	}
	v, err1 := strconv.ParseUint(parts[1], 10, 32)
	u, err2 := strconv.ParseUint(parts[2], 10, 32)
	if err1 != nil || err2 != nil || v == 0 || u == 0 {
		return "", 0, 0, errNotFound
	}
	return parts[0], uint32(v), uint32(u), nil
}

func makeID(role string, validity, uid uint32) string {
	return role + "-" + strconv.FormatUint(uint64(validity), 10) + "-" + strconv.FormatUint(uint64(uid), 10)
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

// previewLimit: bigger messages are listed (and reported as new) from
// their headers only.
const previewLimit = 256 << 10

// toMessage reads a fetched message (with whole body or header).
func (b *imapBackend) toMessage(role string, validity uint32, fm *imap.Message, section *imap.BodySectionName) *Message {
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
	m.ID = makeID(role, validity, fm.Uid)
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

// fetchMessages fetches uids as Messages: whole when small, headers (and
// structure) when big. Missing uids are left out.
func (b *imapBackend) fetchMessages(c *client.Client, role string, validity uint32, uids []uint32) (map[uint32]*Message, error) {
	meta, err := fetch(c, uidSet(uids...), []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size})
	if err != nil {
		return nil, err
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
			return nil, err
		}
		for _, fm := range msgs {
			if slices.Contains(fm.Flags, imap.DeletedFlag) {
				continue // on its way out (see remove)
			}
			m := b.toMessage(role, validity, fm, part.section)
			m.Draft = m.Draft || slices.Contains(fm.Flags, imap.DraftFlag)
			got[fm.Uid] = m
		}
	}
	return got, nil
}

func (b *imapBackend) List(ctx context.Context, folder string, limit int, unreadOnly bool, since time.Time) ([]Summary, error) {
	var out []Summary
	err := b.with(func(c *client.Client) error {
		name, _ := b.box(c, folder, false)
		if name == "" {
			return nil
		}
		st, err := c.Select(name, true)
		if err != nil {
			return err
		}
		crit := imap.NewSearchCriteria()
		crit.WithoutFlags = []string{imap.DeletedFlag}
		if unreadOnly {
			crit.WithoutFlags = append(crit.WithoutFlags, imap.SeenFlag)
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
		got, err := b.fetchMessages(c, folder, st.UidValidity, uids)
		if err != nil {
			return err
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

// open selects the folder an id is in and checks the id's UIDVALIDITY
// (call inside with).
func (b *imapBackend) open(c *client.Client, id string, readOnly bool) (role string, uid uint32, err error) {
	role, validity, uid, err := parseID(id)
	if err != nil {
		return "", 0, err
	}
	name, _ := b.box(c, role, false)
	if name == "" {
		return "", 0, errNotFound
	}
	st, err := c.Select(name, readOnly)
	if err != nil {
		return "", 0, err
	}
	if st.UidValidity != validity {
		return "", 0, errNotFound // the folder was renumbered: that id is gone
	}
	return role, uid, nil
}

// raw fetches one message whole (call inside with).
func (b *imapBackend) raw(c *client.Client, id string, readOnly bool) (*imap.Message, string, error) {
	role, uid, err := b.open(c, id, readOnly)
	if err != nil {
		return nil, "", err
	}
	msgs, err := fetch(c, uidSet(uid), []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, wholeBody.FetchItem()})
	if err != nil {
		return nil, "", err
	}
	for _, m := range msgs {
		if m.Uid == uid && !slices.Contains(m.Flags, imap.DeletedFlag) {
			return m, role, nil
		}
	}
	return nil, "", errNotFound
}

func literal(fm *imap.Message) []byte {
	var buf bytes.Buffer
	if lit := fm.GetBody(wholeBody); lit != nil {
		_, _ = buf.ReadFrom(lit)
	}
	return buf.Bytes()
}

func (b *imapBackend) Get(ctx context.Context, id string) (*Message, error) {
	var m *Message
	err := b.with(func(c *client.Client) error {
		fm, role, err := b.raw(c, id, true)
		if err != nil {
			return err
		}
		_, validity, _, _ := parseID(id)
		m = b.toMessage(role, validity, fm, wholeBody)
		return nil
	})
	return m, err
}

func (b *imapBackend) MarkSeen(ctx context.Context, id string) error {
	return b.with(func(c *client.Client) error {
		_, uid, err := b.open(c, id, false)
		if err != nil {
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
		m, d, err := parseRaw(literal(fm), aid)
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

// errDraftLost: APPEND worked but the new draft's UID couldn't be learned.
// The draft is in Drafts, so saving it again would make a second one.
var errDraftLost = errors.New("the draft was saved in Drafts, but the server didn't say under which id: it is there for a person to see; don't save it again")

// appendUID APPENDs and returns the new message's UIDVALIDITY and UID when
// the server says them (UIDPLUS: [APPENDUID v uid]), else zeros.
func appendUID(c *client.Client, mbox string, flags []string, date time.Time, raw []byte) (uint32, uint32, error) {
	st, err := c.Execute(&commands.Append{Mailbox: mbox, Flags: flags, Date: date, Message: bytes.NewBuffer(raw)}, nil)
	if err != nil {
		return 0, 0, err
	}
	if err := st.Err(); err != nil {
		return 0, 0, err
	}
	if strings.EqualFold(string(st.Code), "APPENDUID") && len(st.Arguments) >= 2 {
		v, err1 := imap.ParseNumber(st.Arguments[0])
		u, err2 := imap.ParseNumber(st.Arguments[1])
		if err1 == nil && err2 == nil && v != 0 && u != 0 {
			return v, u, nil
		}
	}
	return 0, 0, nil
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
		v, u, err := appendUID(c, name, []string{imap.DraftFlag, imap.SeenFlag}, now, raw)
		if err != nil {
			return err // nothing saved
		}
		if u != 0 {
			id = makeID("drafts", v, u)
			return nil
		}
		// No UIDPLUS: find the new draft by its Message-ID. From here on the
		// draft exists, so any failure is errDraftLost, never a plain error
		// that would make the caller save it again.
		st, err := c.Select(name, true)
		if err != nil {
			return fmt.Errorf("%w (%v)", errDraftLost, err)
		}
		crit := imap.NewSearchCriteria()
		crit.Header = textproto.MIMEHeader{"Message-Id": {"<" + out.MessageID + ">"}}
		uids, err := c.UidSearch(crit)
		if err != nil {
			return fmt.Errorf("%w (%v)", errDraftLost, err)
		}
		if len(uids) == 0 {
			return errDraftLost
		}
		id = makeID("drafts", st.UidValidity, slices.Max(uids))
		return nil
	})
	return id, err
}

// keepSent APPENDs a copy of what went out to Sent; a failure is only
// logged (the message went out). Call inside with.
func (b *imapBackend) keepSent(c *client.Client, raw []byte, date time.Time) {
	name, err := b.box(c, "sent", true)
	if err == nil {
		_, _, err = appendUID(c, name, []string{imap.SeenFlag}, date, raw)
	}
	if err != nil {
		log.Printf("mail: %s: sent, but keeping a copy in Sent failed: %s", b.cfg.Sprig, b.cfg.scrub(err))
	}
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
			b.keepSent(c, copyRaw, now)
			return nil
		})
	}
	if err != nil {
		log.Printf("mail: %s: sent, but keeping a copy in Sent failed: %s", b.cfg.Sprig, b.cfg.scrub(err))
	}
	return nil
}

// forSending prepares a draft's original MIME for sending, unchanged but
// for two header fields: the copy for the wire has no Bcc, and both get a
// Message-ID if the draft had none. It returns the wire copy, the copy for
// Sent and the Message-ID (bare).
func forSending(raw []byte, from string) (wire, keep []byte, msgID string, err error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := gmtextproto.ReadHeader(br)
	if err != nil {
		return nil, nil, "", fmt.Errorf("the draft's header can't be read: %w", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return nil, nil, "", err
	}
	mh := mail.Header{Header: message.Header{Header: h}}
	msgID, _ = mh.MessageID()
	if msgID == "" {
		msgID = newMessageID(from)
		mh.SetMessageID(msgID)
	}
	write := func(h gmtextproto.Header) ([]byte, error) {
		var buf bytes.Buffer
		if err := gmtextproto.WriteHeader(&buf, h); err != nil {
			return nil, err
		}
		buf.Write(body)
		return buf.Bytes(), nil
	}
	if keep, err = write(mh.Header.Header); err != nil {
		return nil, nil, "", err
	}
	wh := mh.Header.Header.Copy()
	wh.Del("Bcc")
	wh.Del("Resent-Bcc")
	if wire, err = write(wh); err != nil {
		return nil, nil, "", err
	}
	return wire, keep, cleanLine(msgID), nil
}

// SendDraft sends the draft as it is in Drafts, its original MIME
// (attachments, HTML and all), to the recipients its own header names.
// After the server took it, the draft is removed from Drafts; if that
// fails it is remembered and removed later (and the plugin never sends
// the same draft twice, see draftClaims), so cleanup is retried, not SMTP.
func (b *imapBackend) SendDraft(ctx context.Context, id string, _ *Message) (string, error) {
	var raw []byte
	err := b.with(func(c *client.Client) error {
		fm, role, err := b.raw(c, id, true)
		if err != nil {
			return err
		}
		if role != "drafts" && !slices.Contains(fm.Flags, imap.DraftFlag) {
			return errNotDraft
		}
		raw = literal(fm)
		return nil
	})
	if err != nil {
		return "", err
	}
	m, _, err := parseRaw(raw, "")
	if err != nil {
		return "", err
	}
	rcpts := emails(append(append(append([]Address{}, m.To...), m.Cc...), m.Bcc...))
	if len(rcpts) == 0 {
		return "", fmt.Errorf("%w: it has no recipients", errBadDraft)
	}
	wire, keep, msgID, err := forSending(raw, b.cfg.Address)
	if err != nil {
		return "", err
	}
	if err := smtpSend(ctx, b.cfg, b.tlsFor, b.cfg.Address, rcpts, wire); err != nil {
		return "", err // errUnconfirmed included: the draft stays, nothing more is done
	}
	_ = b.with(func(c *client.Client) error {
		b.keepSent(c, keep, time.Now())
		if err := b.removeDraft(c, id); err != nil {
			log.Printf("mail: %s: sent a draft but couldn't remove it from Drafts (will retry): %s", b.cfg.Sprig, b.cfg.scrub(err))
			if b.cleanup == nil {
				b.cleanup = map[string]bool{}
			}
			b.cleanup[id] = true
		}
		return nil
	})
	return msgID, nil
}

// removeDraft deletes a draft by id (call inside with).
func (b *imapBackend) removeDraft(c *client.Client, id string) error {
	role, uid, err := b.open(c, id, false)
	if err != nil {
		return err
	}
	if role != "drafts" {
		return errNotDraft
	}
	return remove(c, uid)
}

// tidyDrafts removes drafts that were sent but couldn't be removed then
// (call inside with). One that is gone already is done too.
func (b *imapBackend) tidyDrafts(c *client.Client) {
	for id := range b.cleanup {
		if err := b.removeDraft(c, id); err == nil || errors.Is(err, errNotFound) {
			delete(b.cleanup, id)
		}
	}
}

// uidExpunge is UID EXPUNGE (RFC 4315, UIDPLUS): only the given UIDs.
type uidExpunge struct{ set *imap.SeqSet }

func (u uidExpunge) Command() *imap.Command {
	return &imap.Command{Name: "UID", Arguments: []any{imap.RawString("EXPUNGE"), u.set}}
}

// remove flags one message \Deleted and expunges it, and only it (call
// inside with, its mailbox selected read-write). A plain EXPUNGE removes
// every message flagged \Deleted in the mailbox, including ones another
// client flagged and might still undo, so: with UIDPLUS, UID EXPUNGE of
// just this UID; without, EXPUNGE only when this is the only \Deleted
// message, else it is left flagged (hidden by this plugin, and by most
// clients) for whoever expunges next.
func remove(c *client.Client, uid uint32) error {
	if err := c.UidStore(uidSet(uid), imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	if ok, _ := c.Support("UIDPLUS"); ok {
		st, err := c.Execute(uidExpunge{uidSet(uid)}, nil)
		if err != nil {
			return err
		}
		return st.Err()
	}
	crit := imap.NewSearchCriteria()
	crit.WithFlags = []string{imap.DeletedFlag}
	flagged, err := c.UidSearch(crit)
	if err != nil {
		return nil // flagged \Deleted is enough: it no longer shows
	}
	if len(flagged) == 1 && flagged[0] == uid {
		return c.Expunge(nil)
	}
	return nil
}

func (b *imapBackend) Discard(ctx context.Context, id string) error {
	role, _, _, err := parseID(id)
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
		if err := b.removeDraft(c, id); err != nil {
			return err
		}
		delete(b.cleanup, id)
		return nil
	})
}

func (b *imapBackend) Archive(ctx context.Context, id string) error {
	role, _, _, err := parseID(id)
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
		fm, _, err := b.raw(c, id, false)
		if err != nil {
			return err
		}
		// UID MOVE where the server has it, sent by hand: go-imap's UidMove
		// falls back on its own to COPY and a mailbox-wide EXPUNGE, and our
		// fallback after its half-done one would copy the message twice. A
		// MOVE the server answers NO did nothing (RFC 6851), so COPY is then
		// safe; a lost answer is not, so that is an error.
		if ok, _ := c.Support("MOVE"); ok {
			st, err := c.Execute(&commands.Uid{Cmd: &commands.Move{SeqSet: uidSet(fm.Uid), Mailbox: dest}}, nil)
			if err != nil {
				return err
			}
			if st.Err() == nil {
				return nil
			}
		}
		if err := c.UidCopy(uidSet(fm.Uid), dest); err != nil {
			return err
		}
		return remove(c, fm.Uid)
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

// newBatch is how many new messages one round fetches: with bodies of at
// most previewLimit each (bigger ones by their headers), a few MiB.
const newBatch = 20

// checkNew reports inbox messages with a UID not seen yet, a batch at a
// time, moving the cursor on after each batch: a burst of big mail is
// never fetched all at once, and a failure only repeats the unfinished
// batch.
func (b *imapBackend) checkNew(c *client.Client) error {
	b.wmu.Lock()
	next, validity := b.next, b.validity
	b.wmu.Unlock()
	crit := imap.NewSearchCriteria()
	crit.Uid = new(imap.SeqSet)
	crit.Uid.AddRange(next, 0) // next:* (also matches the last message when nothing is new)
	uids, err := c.UidSearch(crit)
	if err != nil {
		return err
	}
	uids = slices.DeleteFunc(uids, func(u uint32) bool { return u < next })
	slices.Sort(uids)
	for start := 0; start < len(uids); start += newBatch {
		chunk := uids[start:min(start+newBatch, len(uids))]
		got, err := b.fetchMessages(c, "inbox", validity, chunk)
		if err != nil {
			return err
		}
		for _, uid := range chunk {
			if m := got[uid]; m != nil && !m.Draft {
				b.onNew(m)
			}
		}
		b.wmu.Lock()
		b.next = chunk[len(chunk)-1] + 1
		b.wmu.Unlock()
	}
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
