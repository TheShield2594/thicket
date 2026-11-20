package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"
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
		prev, _ := maskOTP(m.Subject, m.Preview)
		out = append(out, map[string]any{
			"id": m.ID, "from": m.From.String(), "to": addrStrings(m.To), "subject": maskSubject(m.Subject),
			"date": rfc3339(m.Date), "unread": m.Unread, "attachments": m.Attachments, "preview": prev,
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
	to := orig.ReplyTo
	if len(to) == 0 && orig.From.Email != "" {
		to = []Address{orig.From}
	}
	var cc []Address
	if in.ReplyAll {
		cc = append(append(cc, orig.To...), orig.Cc...)
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
	subject := orig.Subject
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
	ok, release := s.sends.reserve(a.cfg.Sprig, a.cfg.sendLimit(), pol.approved, s.now())
	if !ok {
		return s.saveDraft(ctx, a, be, out, s.limitReason(a))
	}
	if err := be.Send(ctx, out); err != nil {
		release()
		return nil, fmt.Errorf("sending failed: %w", err)
	}
	s.sentEvent(a, out, pol.approved)
	return result("Sent to "+strings.Join(addrStrings(out.recipients()), ", ")+". Message-ID "+bracket(out.MessageID)+".", nil), nil
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

// errNotDraft is returned for an id that isn't in Drafts.
var errNotDraft = errors.New("that message isn't a draft")

// sendDraft sends a draft if the rules allow it. A draft that still can't
// go out stays where it is: nothing new is drafted.
func (s *server) sendDraft(ctx context.Context, a *account, be Backend, id string, pol policy) (map[string]any, error) {
	if id == "" {
		return nil, errors.New("draft_id is required")
	}
	m, err := be.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !m.Draft {
		return nil, errNotDraft
	}
	out := s.draftOutgoing(a, m)
	n := len(out.recipients())
	if n == 0 {
		return nil, errors.New("the draft has no recipients")
	}
	if n > maxRecipients {
		return nil, fmt.Errorf("the draft has %d recipients: at most %d per message", n, maxRecipients)
	}
	reason := blocked(a, out.recipients(), pol)
	release := func() {}
	if reason == "" {
		var ok bool
		if ok, release = s.sends.reserve(a.cfg.Sprig, a.cfg.sendLimit(), pol.approved, s.now()); !ok {
			reason = s.limitReason(a)
		}
	}
	if reason != "" {
		return result(fmt.Sprintf("Not sent: %s. The draft is waiting for a person in \"needs you\".", reason), nil), nil
	}
	if err := be.SendDraft(ctx, id, m); err != nil {
		release()
		return nil, fmt.Errorf("sending failed: %w", err)
	}
	s.sentEvent(a, out, pol.approved)
	return result("Sent to "+strings.Join(addrStrings(out.recipients()), ", ")+". Message-ID "+bracket(out.MessageID)+".", nil), nil
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
