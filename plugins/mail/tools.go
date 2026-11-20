package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Caps on what goes to and comes from a sprig (PROTOCOL.md).
const (
	maxTextToSprig  = 32 << 10
	maxAttachListed = 20
	maxRecipients   = 20
	maxComposeBody  = 64 << 10
	maxSubject      = 300
	previewChars    = 500 // of the body, in thicket/drafted
)

var folders = map[string]bool{"inbox": true, "drafts": true, "sent": true, "archive": true}

// policy is what decides whether a message may go out: from core's _meta
// on a tool call, or a person's send from the dashboard.
type policy struct {
	approved  bool
	draftOnly bool
	allow     []string
}

func (s *server) callTool(ctx context.Context, p toolCall) map[string]any {
	sprig := strings.TrimSpace(p.Meta.Call.Sprig)
	a := s.lookup(sprig)
	if sprig == "" || a == nil {
		return toolError(fmt.Sprintf("%s has no mail address yet; its owner can set one up", titleName(sprig)))
	}
	be, err := a.ready()
	if err != nil {
		return toolError(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pol := policy{approved: p.Meta.Call.Approved, draftOnly: p.Meta.Call.DraftOnly, allow: p.Meta.Mail.Allow}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res map[string]any
	switch p.Name {
	case "list_mail":
		res, err = s.listMail(ctx, a, be, args)
	case "read_mail":
		res, err = s.readMail(ctx, a, be, args)
	case "draft_mail", "send_mail":
		res, err = s.composeTool(ctx, a, be, args, pol, p.Name == "draft_mail")
	case "reply_mail":
		res, err = s.replyMail(ctx, a, be, args, pol)
	case "send_draft":
		var in struct {
			DraftID string `json:"draft_id"`
		}
		if err = json.Unmarshal(args, &in); err == nil {
			res, err = s.sendDraft(ctx, a, be, in.DraftID, pol)
		}
	case "discard_draft":
		var in struct {
			DraftID string `json:"draft_id"`
		}
		if err = json.Unmarshal(args, &in); err == nil {
			err = s.discard(ctx, a, be, in.DraftID)
			res = result("Draft discarded.", nil)
		}
	case "archive_mail":
		var in struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(args, &in); err == nil {
			res, err = s.archive(ctx, a, be, in.ID)
		}
	default:
		return toolError("unknown tool " + cleanLine(p.Name))
	}
	if err != nil {
		return toolError(a.cfg.scrub(err))
	}
	return res
}

// cutoff is the oldest mail the sprig may see.
func (s *server) cutoff(a *account) time.Time {
	return s.now().Add(-time.Duration(a.cfg.retentionDays()) * 24 * time.Hour)
}

// getForSprig reads a message the sprig may see: not older than the
// account's retention.
func (s *server) getForSprig(ctx context.Context, a *account, be Backend, id string) (*Message, error) {
	if id == "" {
		return nil, errors.New("id is required")
	}
	m, err := be.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c := s.cutoff(a); !m.Date.IsZero() && m.Date.Before(c) {
		return nil, fmt.Errorf("message %s is older than %s's %d-day mail window", cleanLine(id), a.cfg.displayName(), a.cfg.retentionDays())
	}
	return m, nil
}

func (s *server) listMail(ctx context.Context, a *account, be Backend, args json.RawMessage) (map[string]any, error) {
	var in struct {
		Folder     string `json:"folder"`
		Limit      int    `json:"limit"`
		UnreadOnly bool   `json:"unread_only"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, errors.New("bad arguments")
	}
	if in.Folder == "" {
		in.Folder = "inbox"
	}
	if !folders[in.Folder] {
		return nil, errors.New("folder must be inbox, drafts, sent or archive")
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	in.Limit = max(1, min(in.Limit, 50))
	list, err := be.List(ctx, in.Folder, in.Limit, in.UnreadOnly, s.cutoff(a))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range list {
		out = append(out, map[string]any{
			"id": m.ID, "from": m.From.String(), "to": addrStrings(m.To), "subject": maskSubject(m.Subject),
			"date": rfc3339(m.Date), "unread": m.Unread, "attachments": m.Attachments, "preview": sprigPreview(m),
		})
	}
	return jsonText(map[string]any{"folder": in.Folder, "messages": out}), nil
}

func (s *server) readMail(ctx context.Context, a *account, be Backend, args json.RawMessage) (map[string]any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, errors.New("bad arguments")
	}
	m, err := s.getForSprig(ctx, a, be, in.ID)
	if err != nil {
		return nil, err
	}
	text, _ := maskOTP(m.Subject, m.Text)
	if cut, ok := truncate(text, maxTextToSprig); ok {
		text = cut + "\n\n[cut: the message is longer than 32 KiB]"
	}
	atts := m.AttachmentList
	more := 0
	if len(atts) > maxAttachListed {
		more = len(atts) - maxAttachListed
		atts = atts[:maxAttachListed]
	}
	listed := []map[string]any{}
	for _, at := range atts {
		listed = append(listed, map[string]any{"name": at.Name, "type": at.Type, "size": at.Size})
	}
	out := map[string]any{
		"id": m.ID, "from": m.From.String(), "to": addrStrings(m.To), "cc": addrStrings(m.Cc),
		"subject": maskSubject(m.Subject), "date": rfc3339(m.Date), "message_id": bracket(m.MessageID),
		"text": text, "attachments": listed,
	}
	if len(m.ReplyTo) > 0 {
		out["reply_to"] = addrStrings(m.ReplyTo)
	}
	if more > 0 {
		out["more_attachments"] = more
	}
	if m.Unread {
		_ = be.MarkSeen(ctx, m.ID)
	}
	return jsonText(out), nil
}

// sprigPreview is a listing's preview with one-time codes hidden. The
// backend masks it when it had the whole text (a code's keyword can be past
// the preview's cut); a preview only the provider made is masked
// conservatively: every number that could be a code.
func sprigPreview(m Summary) string {
	if m.SprigPreview != "" || m.Preview == "" {
		return m.SprigPreview
	}
	return maskDigits(m.Preview)
}

// parseAddrs checks addresses given by a sprig.
func parseAddrs(field string, in []string) ([]Address, error) {
	var out []Address
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := netmail.ParseAddress(s)
		if err != nil || strings.ContainsAny(a.Address, " \r\n<>") {
			return nil, fmt.Errorf("%s: %q isn't an email address", field, cleanLine(s))
		}
		out = append(out, Address{Name: cleanName(a.Name), Email: strings.ToLower(a.Address)})
	}
	return out, nil
}

// newOutgoing checks a message a sprig composes against the caps.
func (s *server) newOutgoing(a *account, to, cc, bcc []string, subject, body string) (*Outgoing, error) {
	out := &Outgoing{From: Address{Name: cleanName(a.cfg.Name), Email: a.cfg.Address}}
	var err error
	if out.To, err = parseAddrs("to", to); err != nil {
		return nil, err
	}
	if out.Cc, err = parseAddrs("cc", cc); err != nil {
		return nil, err
	}
	if out.Bcc, err = parseAddrs("bcc", bcc); err != nil {
		return nil, err
	}
	n := len(out.recipients())
	if n == 0 {
		return nil, errors.New("at least one recipient is required")
	}
	if n > maxRecipients {
		return nil, fmt.Errorf("%d recipients: at most %d per message", n, maxRecipients)
	}
	out.Subject = cleanLine(subject)
	if utf8.RuneCountInString(out.Subject) > maxSubject {
		return nil, fmt.Errorf("subject is longer than %d characters", maxSubject)
	}
	if len(body) > maxComposeBody {
		return nil, errors.New("body is longer than 64 KiB")
	}
	out.Body = strings.ToValidUTF8(body, "�")
	out.MessageID = newMessageID(a.cfg.Address)
	return out, nil
}

func (s *server) composeTool(ctx context.Context, a *account, be Backend, args json.RawMessage, pol policy, draftOnly bool) (map[string]any, error) {
	var in struct {
		To        []string `json:"to"`
		Cc        []string `json:"cc"`
		Bcc       []string `json:"bcc"`
		Subject   string   `json:"subject"`
		Body      string   `json:"body"`
		InReplyTo string   `json:"in_reply_to"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, errors.New("bad arguments")
	}
	out, err := s.newOutgoing(a, in.To, in.Cc, in.Bcc, in.Subject, in.Body)
	if err != nil {
		return nil, err
	}
	if draftOnly && strings.TrimSpace(in.InReplyTo) != "" {
		ref := strings.TrimSpace(in.InReplyTo)
		if strings.HasPrefix(ref, "<") {
			out.InReplyTo = cleanLine(strings.Trim(ref, "<>"))
			out.References = []string{out.InReplyTo}
		} else {
			orig, err := s.getForSprig(ctx, a, be, ref)
			if err != nil {
				return nil, err
			}
			threadOnto(out, orig)
		}
	}
	if draftOnly {
		return s.saveDraft(ctx, a, be, out, a.cfg.displayName()+" saved this as a draft")
	}
	return s.deliver(ctx, a, be, out, pol)
}

// threadOnto makes out an answer to orig: In-Reply-To and References.
func threadOnto(out *Outgoing, orig *Message) {
	if orig.MessageID == "" {
		return
	}
	out.InReplyTo = orig.MessageID
	out.References = append(append([]string{}, orig.References...), orig.MessageID)
}

func (s *server) replyMail(ctx context.Context, a *account, be Backend, args json.RawMessage, pol policy) (map[string]any, error) {
	var in struct {
		ID       string `json:"id"`
		Body     string `json:"body"`
		ReplyAll bool   `json:"reply_all"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, errors.New("bad arguments")
	}
	orig, err := s.getForSprig(ctx, a, be, in.ID)
	if err != nil {
		return nil, err
	}
	// Reply-To if it names a real address, else From: an empty or broken
	// Reply-To must not stop the reply from reaching the sender.
	to := validAddrs(orig.ReplyTo)
	if len(to) == 0 {
		to = validAddrs([]Address{orig.From})
	}
	var cc []Address
	if in.ReplyAll {
		cc = validAddrs(append(append(cc, orig.To...), orig.Cc...))
	}
	// Never to ourselves, never twice.
	seen := map[string]bool{strings.ToLower(a.cfg.Address): true}
	pick := func(l []Address) []string {
		var out []string
		for _, ad := range l {
			e := strings.ToLower(ad.Email)
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, ad.Email)
		}
		return out
	}
	toS, ccS := pick(to), pick(cc)
	if len(toS) == 0 {
		toS, ccS = ccS, nil
	}
	// The subject goes into the drafted _meta, core's "needs you" title and
	// back to the sprig: a one-time code in it stays hidden.
	subject := maskSubject(orig.Subject)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), "re:") {
		subject = "Re: " + subject
	}
	if utf8.RuneCountInString(subject) > maxSubject {
		subject = firstRunes(subject, maxSubject)
	}
	out, err := s.newOutgoing(a, toS, ccS, nil, subject, in.Body)
	if err != nil {
		return nil, err
	}
	threadOnto(out, orig)
	return s.deliver(ctx, a, be, out, pol)
}

// validAddrs keeps the addresses that parse as one.
func validAddrs(l []Address) []Address {
	var out []Address
	for _, a := range l {
		if p, err := netmail.ParseAddress(a.Email); err == nil && p.Address != "" && !strings.ContainsAny(p.Address, " \r\n<>") {
			out = append(out, a)
		}
	}
	return out
}

// allowed reports whether an address is on the allowlist: an exact
// address, or "@domain" for that domain only (not its subdomains).
func allowed(addr string, allow []string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return false
	}
	domain := addr[at:]
	for _, e := range allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if e == addr || (strings.HasPrefix(e, "@") && e == domain) {
			return true
		}
	}
	return false
}

// blocked says why a message can't go out without a person's OK, or ""
// if the rules let it (the daily limit is checked separately, when the
// send is counted).
func blocked(a *account, recipients []Address, pol policy) string {
	if pol.approved {
		return ""
	}
	if pol.draftOnly {
		return a.cfg.displayName() + " is at draft level for mail: it may only draft"
	}
	for _, r := range recipients {
		if !allowed(r.Email, pol.allow) {
			return r.Email + " isn't on " + a.cfg.displayName() + "'s mail allowlist"
		}
	}
	return ""
}

func (s *server) limitReason(a *account) string {
	return fmt.Sprintf("%s reached its daily send limit (%d in 24 hours)", a.cfg.displayName(), a.cfg.sendLimit())
}

// deliver sends a new message if the rules allow it, else drafts it.
func (s *server) deliver(ctx context.Context, a *account, be Backend, out *Outgoing, pol policy) (map[string]any, error) {
	if reason := blocked(a, out.recipients(), pol); reason != "" {
		return s.saveDraft(ctx, a, be, out, reason)
	}
	release, err := s.sends.reserve(a.cfg.Sprig, a.cfg.sendLimit(), pol.approved, s.now())
	if err != nil {
		return s.saveDraft(ctx, a, be, out, s.reserveReason(a, err))
	}
	if err := be.Send(ctx, out); err != nil {
		if errors.Is(err, errUnconfirmed) {
			// It may have gone out: the send keeps its count.
			return nil, fmt.Errorf("sending to %s: %w", strings.Join(emails(out.recipients()), ", "), err)
		}
		release()
		return nil, fmt.Errorf("sending failed: %w", err)
	}
	s.sentEvent(a, out, pol.approved)
	return result("Sent to "+strings.Join(addrStrings(out.recipients()), ", ")+". Message-ID "+bracket(out.MessageID)+".", nil), nil
}

// reserveReason says why the send count refused a send.
func (s *server) reserveReason(a *account, err error) string {
	if errors.Is(err, errLimit) {
		return s.limitReason(a)
	}
	return err.Error()
}

func (s *server) sentEvent(a *account, out *Outgoing, approved bool) {
	s.events.add(Event{
		Sprig: a.cfg.Sprig, Kind: "sent", From: out.From.Email, To: emails(out.To), Cc: emails(out.Cc), Bcc: emails(out.Bcc),
		Subject: out.Subject, Size: int64(len(out.Body)), MessageID: bracket(out.MessageID), Approved: &approved,
	})
}

// saveDraft keeps a message in Drafts and says why it wasn't sent.
func (s *server) saveDraft(ctx context.Context, a *account, be Backend, out *Outgoing, reason string) (map[string]any, error) {
	id, err := be.SaveDraft(ctx, out)
	if err != nil {
		return nil, fmt.Errorf("saving the draft failed: %w", err)
	}
	s.events.add(Event{
		Sprig: a.cfg.Sprig, Kind: "drafted", DraftID: id, From: out.From.Email, To: emails(out.To), Cc: emails(out.Cc),
		Bcc: emails(out.Bcc), Subject: out.Subject, Size: int64(len(out.Body)), Reason: reason,
	})
	meta := map[string]any{"thicket/drafted": map[string]any{
		"draft_id": id, "from": out.From.Email, "to": nonNil(emails(out.To)), "cc": nonNil(emails(out.Cc)), "bcc": nonNil(emails(out.Bcc)),
		"subject": out.Subject, "preview": firstRunes(out.Body, previewChars), "reason": reason,
	}}
	return result(fmt.Sprintf("Not sent: saved as draft %s because %s. A person can send it from \"needs you\".", id, reason), meta), nil
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// draftOutgoing rebuilds a draft as an Outgoing, to check it and name it.
func (s *server) draftOutgoing(a *account, m *Message) *Outgoing {
	return &Outgoing{
		From: Address{Name: cleanName(a.cfg.Name), Email: a.cfg.Address},
		To:   m.To, Cc: m.Cc, Bcc: m.Bcc, Subject: m.Subject, Body: m.Text,
		InReplyTo: firstOr(m.InReplyTo), References: m.References, MessageID: m.MessageID,
	}
}

func firstOr(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

var (
	// errNotDraft is returned for an id that isn't in Drafts.
	errNotDraft = errors.New("that message isn't a draft")
	// errDraftSent: this draft was already sent (or handed to the server
	// unconfirmed); it is never sent twice.
	errDraftSent = errors.New("that draft was already sent (or may have been: check Sent); it won't be sent again")
	// errDraftBusy: the draft is being sent right now by another call.
	errDraftBusy = errors.New("that draft is being sent right now")
	// errBadDraft: the draft can't be sent as it is (no recipients, too many).
	errBadDraft = errors.New("the draft can't be sent")
)

// draftClaims makes sure one draft is sent once: the sprig's send_draft
// (a person's "Do it") and a person's send from the dashboard share it. A
// draft is claimed from the moment it is read and checked until it is
// handed to the server; a definite failure gives the claim back, a send
// (confirmed or not) keeps it, so a draft whose cleanup failed, or whose
// submission wasn't confirmed, isn't sent again by a later call.
type draftClaims struct {
	mu   sync.Mutex
	busy map[string]bool
	done map[string]time.Time
}

// keepSent is how long a sent draft's id is remembered. Ids aren't reused
// (IMAP ids carry UIDVALIDITY), so this only bounds memory.
const keepSent = 7 * 24 * time.Hour

func claimKey(sprig, id string) string { return sprig + "\x00" + id }

func (d *draftClaims) claim(sprig, id string, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy == nil {
		d.busy, d.done = map[string]bool{}, map[string]time.Time{}
	}
	for k, t := range d.done {
		if now.Sub(t) > keepSent {
			delete(d.done, k)
		}
	}
	k := claimKey(sprig, id)
	if _, ok := d.done[k]; ok {
		return errDraftSent
	}
	if d.busy[k] {
		return errDraftBusy
	}
	d.busy[k] = true
	return nil
}

// end gives the claim back (sent false) or marks the draft sent.
func (d *draftClaims) end(sprig, id string, sent bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := claimKey(sprig, id)
	delete(d.busy, k)
	if sent {
		d.done[k] = now
	}
}

// sendDraftCore sends a draft if the rules allow it: the one path for the
// sprig's send_draft and a person's send from the dashboard. forSprig
// applies the retention window (a person's approval of that very draft
// lifts it). It returns the draft as sent and the Message-ID the server
// got, or a reason it wasn't sent (the draft stays where it is).
func (s *server) sendDraftCore(ctx context.Context, a *account, be Backend, id string, pol policy, forSprig bool) (out *Outgoing, msgID, reason string, err error) {
	if id == "" {
		return nil, "", "", errors.New("draft_id is required")
	}
	if err := s.drafts.claim(a.cfg.Sprig, id, s.now()); err != nil {
		return nil, "", "", err
	}
	sent := false
	defer func() { s.drafts.end(a.cfg.Sprig, id, sent, s.now()) }()
	var m *Message
	if forSprig && !pol.approved {
		m, err = s.getForSprig(ctx, a, be, id)
	} else {
		m, err = be.Get(ctx, id)
	}
	if err != nil {
		return nil, "", "", err
	}
	if !m.Draft {
		return nil, "", "", errNotDraft
	}
	out = s.draftOutgoing(a, m)
	n := len(out.recipients())
	if n == 0 {
		return nil, "", "", fmt.Errorf("%w: it has no recipients", errBadDraft)
	}
	if n > maxRecipients {
		return nil, "", "", fmt.Errorf("%w: it has %d recipients, at most %d per message", errBadDraft, n, maxRecipients)
	}
	if reason := blocked(a, out.recipients(), pol); reason != "" {
		return out, "", reason, nil
	}
	release, err := s.sends.reserve(a.cfg.Sprig, a.cfg.sendLimit(), pol.approved, s.now())
	if err != nil {
		return out, "", s.reserveReason(a, err), nil
	}
	msgID, err = be.SendDraft(ctx, id, m)
	if err != nil {
		if errors.Is(err, errUnconfirmed) {
			sent = true // may have gone out: never again, and it counts
			return nil, "", "", err
		}
		release()
		return nil, "", "", fmt.Errorf("sending failed: %w", err)
	}
	sent = true
	out.MessageID = msgID
	s.sentEvent(a, out, pol.approved)
	return out, msgID, "", nil
}

// sendDraft is the send_draft tool. A draft that still can't go out stays
// where it is: nothing new is drafted.
func (s *server) sendDraft(ctx context.Context, a *account, be Backend, id string, pol policy) (map[string]any, error) {
	out, msgID, reason, err := s.sendDraftCore(ctx, a, be, id, pol, true)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return result(fmt.Sprintf("Not sent: %s. The draft is waiting for a person in \"needs you\".", reason), nil), nil
	}
	return result("Sent to "+strings.Join(addrStrings(out.recipients()), ", ")+". Message-ID "+bracket(msgID)+".", nil), nil
}

func (s *server) discard(ctx context.Context, a *account, be Backend, id string) error {
	if id == "" {
		return errors.New("draft_id is required")
	}
	m, err := be.Get(ctx, id)
	if err != nil {
		return err
	}
	if !m.Draft {
		return errNotDraft
	}
	// Not while it is being sent.
	switch err := s.drafts.claim(a.cfg.Sprig, id, s.now()); {
	case errors.Is(err, errDraftSent): // sent, but still in Drafts: removing it is fine
	case err != nil:
		return err
	default:
		defer s.drafts.end(a.cfg.Sprig, id, false, s.now())
	}
	if err := be.Discard(ctx, id); err != nil {
		return err
	}
	s.events.add(Event{Sprig: a.cfg.Sprig, Kind: "discarded", DraftID: id, To: emails(m.To), Subject: m.Subject})
	return nil
}

func (s *server) archive(ctx context.Context, a *account, be Backend, id string) (map[string]any, error) {
	m, err := s.getForSprig(ctx, a, be, id)
	if err != nil {
		return nil, err
	}
	if err := be.Archive(ctx, id); err != nil {
		return nil, err
	}
	s.events.add(Event{Sprig: a.cfg.Sprig, Kind: "archived", ID: id, From: m.From.Email, Subject: m.Subject, MessageID: bracket(m.MessageID)})
	return result("Archived.", nil), nil
}

func addrStrings(l []Address) []string {
	out := make([]string, 0, len(l))
	for _, a := range l {
		out = append(out, a.String())
	}
	return out
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
