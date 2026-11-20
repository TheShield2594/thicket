package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode legacy charsets in received mail
	"github.com/emersion/go-message/mail"
)

// newMessageID makes a bare Message-ID on the sender's domain.
func newMessageID(from string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	domain := "thicket.invalid"
	if at := strings.LastIndexByte(from, '@'); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	return hex.EncodeToString(b) + "@" + domain
}

// composeRaw writes an outgoing message as RFC 5322 text/plain UTF-8,
// quoted-printable. withBcc keeps the Bcc header (drafts and the Sent
// copy); what goes over SMTP never has it.
func composeRaw(out *Outgoing, withBcc bool, date time.Time) ([]byte, error) {
	var h mail.Header
	h.SetDate(date)
	h.SetAddressList("From", mailAddrs([]Address{out.From}))
	if len(out.To) > 0 {
		h.SetAddressList("To", mailAddrs(out.To))
	}
	if len(out.Cc) > 0 {
		h.SetAddressList("Cc", mailAddrs(out.Cc))
	}
	if withBcc && len(out.Bcc) > 0 {
		h.SetAddressList("Bcc", mailAddrs(out.Bcc))
	}
	h.SetSubject(cleanLine(out.Subject))
	h.SetMessageID(out.MessageID)
	if out.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{out.InReplyTo})
	}
	if len(out.References) > 0 {
		h.SetMsgIDList("References", out.References)
	}
	h.Set("MIME-Version", "1.0")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	var buf bytes.Buffer
	w, err := mail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, strings.ReplaceAll(out.Body, "\r\n", "\n")); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mailAddrs(l []Address) []*mail.Address {
	out := make([]*mail.Address, 0, len(l))
	for _, a := range l {
		out = append(out, &mail.Address{Name: a.Name, Address: a.Email})
	}
	return out
}

// maxTextPart bounds how much of one text part is read.
const maxTextPart = 4 << 20

// maxAttachment is the most of one attachment the plugin hands out (a
// variable so tests can shrink it).
var maxAttachment = 50 << 20

// errTooBig: an attachment is bigger than the plugin hands out. Its bytes
// are never cut short and passed off as the whole file.
var errTooBig = errors.New("the attachment is too big to download here (more than 50 MiB)")

// maxMIMEDepth bounds how deep multiparts nest before the rest is ignored.
const maxMIMEDepth = 16

// parseRaw reads a whole RFC 5322 message: headers, the text and the
// attachment list. The text follows the MIME tree: within a
// multipart/alternative, text/plain is preferred over HTML (converted);
// within any other multipart (mixed, related, ...) every inline text part
// counts, in order. With wantAID it also returns that attachment's bytes.
func parseRaw(raw []byte, wantAID string) (*Message, []byte, error) {
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil {
		return nil, nil, fmt.Errorf("unreadable message: %w", err)
	}
	h := mail.Header{Header: e.Header}
	m := &Message{}
	m.From = firstAddr(addrList(h, "From"))
	m.To = addrList(h, "To")
	m.Cc = addrList(h, "Cc")
	m.Bcc = addrList(h, "Bcc")
	m.ReplyTo = addrList(h, "Reply-To")
	if subj, err := h.Subject(); err == nil {
		m.Subject = cleanLine(subj)
	} else {
		m.Subject = cleanLine(h.Get("Subject"))
	}
	m.Date, _ = h.Date()
	m.MessageID, _ = h.MessageID()
	m.MessageID = cleanLine(m.MessageID)
	m.InReplyTo, _ = h.MsgIDList("In-Reply-To")
	m.References, _ = h.MsgIDList("References")

	w := &mimeWalk{m: m, want: wantAID}
	m.Text, _ = w.entity(e, 0)
	if w.wantErr != nil {
		return nil, nil, w.wantErr
	}
	m.Attachments = len(m.AttachmentList)
	m.Size = int64(len(raw))
	m.Preview, m.SprigPreview = previewsOf(m.Subject, m.Text)
	return m, w.wanted, nil
}

// mimeWalk goes through a message's MIME tree once.
type mimeWalk struct {
	m         *Message
	want      string // the attachment id whose bytes are wanted
	wanted    []byte
	wantErr   error
	n         int // attachments numbered in tree order: their ids
	textBytes int // text read so far, all parts together
}

// Kinds of text a subtree gave, for multipart/alternative's choice.
const (
	kindNone = iota
	kindHTML
	kindPlain
)

// entity returns the text of e's subtree and what kind it is; attachments
// on the way are added to the message.
func (w *mimeWalk) entity(e *message.Entity, depth int) (string, int) {
	if mr := e.MultipartReader(); mr != nil {
		if depth >= maxMIMEDepth {
			return "", kindNone
		}
		ct, _, _ := e.Header.ContentType()
		type alt struct {
			text string
			kind int
		}
		var parts []alt
		for {
			p, err := mr.NextPart()
			if p == nil || (err != nil && errors.Is(err, io.EOF)) {
				break // the end, or a broken part that ends what we can read
			}
			t, k := w.entity(p, depth+1)
			parts = append(parts, alt{t, k})
		}
		if ct == "multipart/alternative" {
			for _, want := range []int{kindPlain, kindHTML} {
				for _, p := range parts {
					if p.kind == want && p.text != "" {
						return p.text, p.kind
					}
				}
			}
			return "", kindNone
		}
		var texts []string
		kind := kindNone
		for _, p := range parts {
			if p.text != "" {
				texts = append(texts, p.text)
				kind = max(kind, p.kind)
			}
		}
		return strings.Join(texts, "\n\n"), kind
	}

	ct, params, _ := e.Header.ContentType()
	if ct == "" {
		ct = "text/plain"
	}
	disp, dparams, _ := e.Header.ContentDisposition()
	if strings.EqualFold(disp, "attachment") {
		w.n++
		name := firstNonEmpty(dparams["filename"], params["name"])
		if name == "" {
			name = "attachment-" + strconv.Itoa(w.n)
		}
		w.attachment(e.Body, name, ct)
		return "", kindNone
	}
	switch {
	case ct == "text/plain" || ct == "text/html":
		if w.textBytes >= maxTextPart {
			return "", kindNone // enough text already
		}
		b, _ := io.ReadAll(io.LimitReader(e.Body, maxTextPart))
		w.textBytes += len(b)
		if ct == "text/plain" {
			return tidyPlain(string(b)), kindPlain
		}
		return htmlToText(string(b)), kindHTML
	case strings.HasPrefix(ct, "text/"):
		return "", kindNone // another rendering of the same text (calendar, enriched...)
	}
	w.n++
	name := params["name"]
	if name == "" {
		name = "inline-" + strconv.Itoa(w.n)
	}
	w.attachment(e.Body, name, ct)
	return "", kindNone
}

// attachment lists one attachment, and keeps its bytes when it is the one
// wanted (whole, or errTooBig).
func (w *mimeWalk) attachment(body io.Reader, name, ct string) {
	if ct == "" {
		ct = "application/octet-stream"
	}
	id := strconv.Itoa(w.n)
	var size int64
	if id == w.want {
		b, _ := io.ReadAll(io.LimitReader(body, int64(maxAttachment)+1))
		if len(b) > maxAttachment {
			w.wantErr = errTooBig
			b = nil
		}
		w.wanted, size = b, int64(len(b))
	} else {
		size, _ = io.Copy(io.Discard, body)
	}
	w.m.AttachmentList = append(w.m.AttachmentList, Attachment{ID: id, Name: cleanLine(name), Type: cleanLine(ct), Size: size})
}

// tidyPlain cleans a text/plain body: control characters out, lines kept.
func tidyPlain(s string) string {
	return strings.TrimSpace(cleanText(s))
}

// preview is a one-line start of the text.
func preview(text string) string {
	return firstRunes(strings.Join(strings.Fields(text), " "), 200)
}

func addrList(h mail.Header, key string) []Address {
	list, err := h.AddressList(key)
	if err != nil {
		// Unparseable: keep what looks like addresses, as text only.
		raw := cleanLine(h.Get(key))
		if raw == "" {
			return nil
		}
		var out []Address
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, Address{Email: cleanName(p)})
			}
		}
		return out
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		out = append(out, Address{Name: cleanName(a.Name), Email: strings.ToLower(cleanName(a.Address))})
	}
	return out
}

func firstAddr(l []Address) Address {
	if len(l) == 0 {
		return Address{}
	}
	return l[0]
}
