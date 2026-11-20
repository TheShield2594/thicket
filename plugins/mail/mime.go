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

// parseRaw reads a whole RFC 5322 message: headers, the text (text/plain
// preferred, else the HTML converted), and the attachment list. With
// wantAID it also returns that attachment's bytes.
func parseRaw(raw []byte, wantAID string) (*Message, []byte, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return nil, nil, fmt.Errorf("unreadable message: %w", err)
	}
	h := mr.Header
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

	var plain, htm string
	havePlain, haveHTML := false, false
	var wanted []byte
	n := 0
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if p == nil {
				break // a broken part ends what we can read
			}
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, params, _ := ph.ContentType()
			if ct == "" {
				ct = "text/plain"
			}
			if ct == "text/plain" && !havePlain {
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxTextPart))
				plain, havePlain = string(b), true
				continue
			}
			if ct == "text/html" && !haveHTML {
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxTextPart))
				htm, haveHTML = string(b), true
				continue
			}
			if strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "multipart/") {
				continue // another alternative of the same text
			}
			n++
			name := params["name"]
			if name == "" {
				name = "inline-" + strconv.Itoa(n)
			}
			wanted = addAttachment(m, p.Body, strconv.Itoa(n), name, ct, wantAID, wanted)
		case *mail.AttachmentHeader:
			n++
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			if name == "" {
				name = "attachment-" + strconv.Itoa(n)
			}
			wanted = addAttachment(m, p.Body, strconv.Itoa(n), name, ct, wantAID, wanted)
		}
	}
	switch {
	case havePlain:
		m.Text = tidyPlain(plain)
	case haveHTML:
		m.Text = htmlToText(htm)
	}
	m.Attachments = len(m.AttachmentList)
	m.Size = int64(len(raw))
	m.Preview = preview(m.Text)
	return m, wanted, nil
}

func addAttachment(m *Message, body io.Reader, id, name, ct, want string, wanted []byte) []byte {
	if ct == "" {
		ct = "application/octet-stream"
	}
	var size int64
	if id == want {
		b, _ := io.ReadAll(io.LimitReader(body, 50<<20))
		wanted, size = b, int64(len(b))
	} else {
		size, _ = io.Copy(io.Discard, body)
	}
	m.AttachmentList = append(m.AttachmentList, Attachment{ID: id, Name: cleanLine(name), Type: cleanLine(ct), Size: size})
	return wanted
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
