package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The JMAP backend (RFC 8620, 8621), written by hand: JMAP is JSON over
// HTTPS, so a small client is clearer than a library. Every URL must be
// https; certificates are always verified.

var errNotFound = errors.New("no such message")

const (
	jmapCore       = "urn:ietf:params:jmap:core"
	jmapMail       = "urn:ietf:params:jmap:mail"
	jmapSubmission = "urn:ietf:params:jmap:submission"
)

type jmapBackend struct {
	cfg    AccountConfig
	timing timing
	onNew  func(*Message)
	hc     *http.Client // requests, with a timeout
	stream *http.Client // EventSource, no overall timeout

	mu       sync.Mutex
	sess     *jmapSession
	acct     string
	boxes    map[string]string // role -> mailbox id
	identity string
	push     string // what /v1/doctor reports
	state    string // the Email state already seen; "" until first connect
}

type jmapSession struct {
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
}

func newJMAP(cfg AccountConfig, tlsFor func(string) *tls.Config, t timing, onNew func(*Message)) *jmapBackend {
	tr := &http.Transport{TLSClientConfig: tlsFor(""), ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}
	httpsOnly := func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("refusing a redirect away from https")
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &jmapBackend{
		cfg: cfg, timing: t, onNew: onNew,
		hc:     &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: httpsOnly},
		stream: &http.Client{Transport: tr, CheckRedirect: httpsOnly},
	}
}

func (j *jmapBackend) auth(req *http.Request) {
	if j.cfg.Username == "" {
		req.Header.Set("Authorization", "Bearer "+j.cfg.Password)
	} else {
		req.SetBasicAuth(j.cfg.Username, j.cfg.Password)
	}
}

func httpsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("JMAP server gave a URL that isn't https: %q", cleanLine(raw))
	}
	return nil
}

func (j *jmapBackend) fetchSession(ctx context.Context) (*jmapSession, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", j.cfg.JMAP.SessionURL, nil)
	if err != nil {
		return nil, err
	}
	j.auth(req)
	req.Header.Set("Accept", "application/json")
	resp, err := j.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JMAP session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("login failed: the JMAP server refused the username or password")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JMAP session: HTTP %d", resp.StatusCode)
	}
	var s jmapSession
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
		return nil, fmt.Errorf("JMAP session: %w", err)
	}
	if _, ok := s.Capabilities[jmapMail]; !ok {
		return nil, errors.New("JMAP server doesn't offer mail")
	}
	if s.PrimaryAccounts[jmapMail] == "" {
		return nil, errors.New("JMAP session names no mail account")
	}
	for _, u := range []string{s.APIURL, s.DownloadURL} {
		if err := httpsURL(u); err != nil {
			return nil, err
		}
	}
	if s.EventSourceURL != "" && httpsURL(s.EventSourceURL) != nil {
		s.EventSourceURL = "" // not https: poll instead
	}
	return &s, nil
}

type jcall struct {
	name string
	args map[string]any
}

// call sends method calls in one request and returns each answer's
// arguments. A method error fails the whole call.
func (j *jmapBackend) call(ctx context.Context, calls ...jcall) ([]json.RawMessage, error) {
	j.mu.Lock()
	sess, acct := j.sess, j.acct
	j.mu.Unlock()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	mc := make([]any, len(calls))
	for i, c := range calls {
		if _, ok := c.args["accountId"]; !ok {
			c.args["accountId"] = acct
		}
		mc[i] = []any{c.name, c.args, fmt.Sprint(i)}
	}
	body, _ := json.Marshal(map[string]any{"using": []string{jmapCore, jmapMail, jmapSubmission}, "methodCalls": mc})
	req, err := http.NewRequestWithContext(ctx, "POST", sess.APIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	j.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JMAP: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JMAP: HTTP %d", resp.StatusCode)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("JMAP: %w", err)
	}
	if len(out.MethodResponses) != len(calls) {
		return nil, errors.New("JMAP: unexpected answer")
	}
	args := make([]json.RawMessage, len(calls))
	for i, r := range out.MethodResponses {
		if len(r) < 2 {
			return nil, errors.New("JMAP: unexpected answer")
		}
		var name string
		_ = json.Unmarshal(r[0], &name)
		if name == "error" {
			var e jmapError
			_ = json.Unmarshal(r[1], &e)
			return nil, &e
		}
		args[i] = r[1]
	}
	return args, nil
}

type jmapError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e *jmapError) Error() string {
	if e.Description != "" {
		return "JMAP " + e.Type + ": " + cleanLine(e.Description)
	}
	return "JMAP " + e.Type
}

func (j *jmapBackend) call1(ctx context.Context, name string, args map[string]any, out any) error {
	res, err := j.call(ctx, jcall{name, args})
	if err != nil {
		return err
	}
	return json.Unmarshal(res[0], out)
}

func (j *jmapBackend) Connect(ctx context.Context) error {
	sess, err := j.fetchSession(ctx)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.sess, j.acct = sess, sess.PrimaryAccounts[jmapMail]
	j.mu.Unlock()

	var mb struct {
		List []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"list"`
	}
	if err := j.call1(ctx, "Mailbox/get", map[string]any{"ids": nil, "properties": []string{"id", "name", "role"}}, &mb); err != nil {
		return err
	}
	boxes := map[string]string{}
	for _, m := range mb.List {
		if m.Role != "" {
			boxes[strings.ToLower(m.Role)] = m.ID
		}
	}
	for _, m := range mb.List { // servers without roles: go by name
		for _, role := range []string{"drafts", "sent", "archive", "trash"} {
			if boxes[role] == "" && strings.EqualFold(m.Name, role) {
				boxes[role] = m.ID
			}
		}
	}
	for _, role := range []string{"inbox", "drafts", "sent"} {
		if boxes[role] == "" {
			return fmt.Errorf("JMAP account has no %s mailbox", role)
		}
	}
	j.mu.Lock()
	j.boxes = boxes
	baseline := j.state == ""
	j.mu.Unlock()
	if baseline {
		// Whatever is there now is not new mail.
		st, err := j.emailState(ctx)
		if err != nil {
			return err
		}
		j.mu.Lock()
		j.state = st
		j.mu.Unlock()
	}
	return nil
}

func (j *jmapBackend) emailState(ctx context.Context) (string, error) {
	var r struct {
		State string `json:"state"`
	}
	err := j.call1(ctx, "Email/get", map[string]any{"ids": []string{}, "properties": []string{"id"}}, &r)
	return r.State, err
}

func (j *jmapBackend) box(role string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.boxes[role]
}

type jAddr struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type jPart struct {
	PartID      string `json:"partId"`
	BlobID      string `json:"blobId"`
	Size        int64  `json:"size"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Disposition string `json:"disposition"`
}

type jEmail struct {
	ID         string          `json:"id"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	From       []jAddr         `json:"from"`
	To         []jAddr         `json:"to"`
	Cc         []jAddr         `json:"cc"`
	Bcc        []jAddr         `json:"bcc"`
	ReplyTo    []jAddr         `json:"replyTo"`
	Subject    string          `json:"subject"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Size       int64           `json:"size"`
	Preview    string          `json:"preview"`
	MessageID  []string        `json:"messageId"`
	InReplyTo  []string        `json:"inReplyTo"`
	References []string        `json:"references"`
	TextBody   []jPart         `json:"textBody"`
	HTMLBody   []jPart         `json:"htmlBody"`
	Attach     []jPart         `json:"attachments"`
	BodyValues map[string]struct {
		Value       string `json:"value"`
		IsTruncated bool   `json:"isTruncated"`
	} `json:"bodyValues"`
}

var (
	summaryProps = []string{"id", "mailboxIds", "keywords", "from", "to", "subject", "receivedAt", "size", "preview", "attachments"}
	fullProps    = []string{"id", "mailboxIds", "keywords", "from", "to", "cc", "bcc", "replyTo", "subject", "receivedAt",
		"size", "preview", "messageId", "inReplyTo", "references", "textBody", "htmlBody", "attachments", "bodyValues"}
)

func jAddrs(l []jAddr) []Address {
	out := make([]Address, 0, len(l))
	for _, a := range l {
		out = append(out, Address{Name: cleanName(a.Name), Email: strings.ToLower(cleanName(a.Email))})
	}
	return out
}

func toJAddrs(l []Address) []jAddr {
	out := make([]jAddr, 0, len(l))
	for _, a := range l {
		out = append(out, jAddr{Name: a.Name, Email: a.Email})
	}
	return out
}

func (j *jmapBackend) folderOf(e *jEmail) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, role := range []string{"inbox", "drafts", "sent", "archive"} {
		if id := j.boxes[role]; id != "" && e.MailboxIDs[id] {
			return role
		}
	}
	return "other"
}

func (j *jmapBackend) toMessage(e *jEmail) *Message {
	m := &Message{}
	m.ID = e.ID
	m.Folder = j.folderOf(e)
	m.From = firstAddr(jAddrs(e.From))
	m.To, m.Cc, m.Bcc, m.ReplyTo = jAddrs(e.To), jAddrs(e.Cc), jAddrs(e.Bcc), jAddrs(e.ReplyTo)
	m.Subject = cleanLine(e.Subject)
	m.Date = e.ReceivedAt
	m.Size = e.Size
	m.Unread = !e.Keywords["$seen"]
	m.Draft = e.Keywords["$draft"] || m.Folder == "drafts"
	if len(e.MessageID) > 0 {
		m.MessageID = cleanLine(e.MessageID[0])
	}
	m.InReplyTo, m.References = e.InReplyTo, e.References
	for _, p := range e.Attach {
		name := p.Name
		if name == "" {
			name = "attachment"
		}
		m.AttachmentList = append(m.AttachmentList, Attachment{ID: p.BlobID, Name: cleanLine(name), Type: cleanLine(p.Type), Size: p.Size})
	}
	m.Attachments = len(m.AttachmentList)
	if e.BodyValues != nil {
		// text/plain parts if there are any, else the HTML converted.
		var plain, htm []string
		truncated := false
		for _, p := range e.TextBody {
			if v, ok := e.BodyValues[p.PartID]; ok && p.Type == "text/plain" {
				plain = append(plain, v.Value)
				truncated = truncated || v.IsTruncated
			}
		}
		if len(plain) > 0 {
			m.Text = tidyPlain(strings.Join(plain, "\n\n"))
		} else {
			for _, p := range e.HTMLBody {
				if v, ok := e.BodyValues[p.PartID]; ok && (p.Type == "text/html" || p.Type == "text/plain") {
					if p.Type == "text/html" {
						htm = append(htm, htmlToText(v.Value))
					} else {
						htm = append(htm, tidyPlain(v.Value))
					}
					truncated = truncated || v.IsTruncated
				}
			}
			m.Text = strings.Join(htm, "\n\n")
		}
		if truncated {
			m.Text += "\n\n[cut: the server sent only the start of this message]"
		}
		m.Preview = preview(m.Text)
	} else {
		m.Preview = preview(cleanLine(e.Preview))
	}
	return m
}

func (j *jmapBackend) getEmails(ctx context.Context, ids []string, props []string) ([]jEmail, error) {
	args := map[string]any{"ids": ids, "properties": props}
	if len(props) == len(fullProps) {
		args["bodyProperties"] = []string{"partId", "blobId", "size", "name", "type", "disposition"}
		args["fetchTextBodyValues"] = true
		args["fetchHTMLBodyValues"] = true
		args["maxBodyValueBytes"] = maxTextPart
	}
	var r struct {
		List []jEmail `json:"list"`
	}
	if err := j.call1(ctx, "Email/get", args, &r); err != nil {
		return nil, err
	}
	return r.List, nil
}

func (j *jmapBackend) List(ctx context.Context, folder string, limit int, unreadOnly bool, since time.Time) ([]Summary, error) {
	box := j.box(folder)
	if box == "" {
		return nil, nil // no such mailbox: nothing in it
	}
	filter := map[string]any{"inMailbox": box}
	if unreadOnly {
		filter["notKeyword"] = "$seen"
	}
	if !since.IsZero() {
		filter["after"] = since.UTC().Format(time.RFC3339)
	}
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := j.call1(ctx, "Email/query", map[string]any{
		"filter": filter, "sort": []any{map[string]any{"property": "receivedAt", "isAscending": false}}, "limit": limit,
	}, &q); err != nil {
		return nil, err
	}
	if len(q.IDs) == 0 {
		return nil, nil
	}
	list, err := j.getEmails(ctx, q.IDs, summaryProps)
	if err != nil {
		return nil, err
	}
	byID := map[string]*jEmail{}
	for i := range list {
		byID[list[i].ID] = &list[i]
	}
	out := []Summary{}
	for _, id := range q.IDs {
		if e := byID[id]; e != nil {
			out = append(out, j.toMessage(e).Summary)
		}
	}
	return out, nil
}

func (j *jmapBackend) Get(ctx context.Context, id string) (*Message, error) {
	list, err := j.getEmails(ctx, []string{id}, fullProps)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errNotFound
	}
	return j.toMessage(&list[0]), nil
}

func (j *jmapBackend) MarkSeen(ctx context.Context, id string) error {
	return j.update(ctx, id, map[string]any{"keywords/$seen": true})
}

func (j *jmapBackend) update(ctx context.Context, id string, patch map[string]any) error {
	var r struct {
		NotUpdated map[string]jmapError `json:"notUpdated"`
	}
	if err := j.call1(ctx, "Email/set", map[string]any{"update": map[string]any{id: patch}}, &r); err != nil {
		return err
	}
	if e, ok := r.NotUpdated[id]; ok {
		return &e
	}
	return nil
}

func (j *jmapBackend) Attachment(ctx context.Context, id, aid string) (Attachment, []byte, error) {
	list, err := j.getEmails(ctx, []string{id}, []string{"id", "attachments"})
	if err != nil {
		return Attachment{}, nil, err
	}
	if len(list) == 0 {
		return Attachment{}, nil, errNotFound
	}
	for _, p := range list[0].Attach {
		if p.BlobID != aid {
			continue
		}
		att := Attachment{ID: p.BlobID, Name: cleanLine(p.Name), Type: cleanLine(p.Type), Size: p.Size}
		j.mu.Lock()
		tmpl, acct := j.sess.DownloadURL, j.acct
		j.mu.Unlock()
		u := strings.NewReplacer(
			"{accountId}", url.PathEscape(acct), "{blobId}", url.PathEscape(p.BlobID),
			"{type}", url.QueryEscape("application/octet-stream"), "{name}", url.PathEscape(firstNonEmpty(p.Name, "attachment")),
		).Replace(tmpl)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return att, nil, err
		}
		j.auth(req)
		resp, err := j.hc.Do(req)
		if err != nil {
			return att, nil, fmt.Errorf("JMAP download: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return att, nil, fmt.Errorf("JMAP download: HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		return att, data, err
	}
	return Attachment{}, nil, errNotFound
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// emailObject is an outgoing message as a JMAP Email to create in Drafts.
func (j *jmapBackend) emailObject(out *Outgoing) map[string]any {
	e := map[string]any{
		"mailboxIds": map[string]bool{j.box("drafts"): true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       toJAddrs([]Address{out.From}),
		"to":         toJAddrs(out.To),
		"subject":    out.Subject,
		"messageId":  []string{out.MessageID},
		"bodyValues": map[string]any{"body": map[string]any{"value": out.Body}},
		"textBody":   []any{map[string]any{"partId": "body", "type": "text/plain"}},
	}
	if len(out.Cc) > 0 {
		e["cc"] = toJAddrs(out.Cc)
	}
	if len(out.Bcc) > 0 {
		e["bcc"] = toJAddrs(out.Bcc)
	}
	if out.InReplyTo != "" {
		e["inReplyTo"] = []string{out.InReplyTo}
	}
	if len(out.References) > 0 {
		e["references"] = out.References
	}
	return e
}

type setResult struct {
	Created    map[string]struct{ ID string } `json:"created"`
	NotCreated map[string]jmapError           `json:"notCreated"`
}

func (j *jmapBackend) SaveDraft(ctx context.Context, out *Outgoing) (string, error) {
	var r setResult
	if err := j.call1(ctx, "Email/set", map[string]any{"create": map[string]any{"d": j.emailObject(out)}}, &r); err != nil {
		return "", err
	}
	if e, ok := r.NotCreated["d"]; ok {
		return "", &e
	}
	return r.Created["d"].ID, nil
}

// identityID finds the sending identity for the account's address.
func (j *jmapBackend) identityID(ctx context.Context) (string, error) {
	j.mu.Lock()
	id := j.identity
	j.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var r struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"list"`
	}
	if err := j.call1(ctx, "Identity/get", map[string]any{"ids": nil}, &r); err != nil {
		return "", err
	}
	addr := strings.ToLower(j.cfg.Address)
	domain := addr[strings.LastIndexByte(addr, '@')+1:]
	for _, want := range []string{addr, "*@" + domain} {
		for _, i := range r.List {
			if strings.ToLower(i.Email) == want {
				id = i.ID
				break
			}
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		return "", fmt.Errorf("no JMAP sending identity for %s", j.cfg.Address)
	}
	j.mu.Lock()
	j.identity = id
	j.mu.Unlock()
	return id, nil
}

// submission is EmailSubmission/set for emailId (an id, or "#d" for one
// created in the same request), moving it to Sent and dropping $draft
// when it succeeds.
func (j *jmapBackend) submission(identity, emailID string) jcall {
	return jcall{"EmailSubmission/set", map[string]any{
		"create": map[string]any{"s": map[string]any{"identityId": identity, "emailId": emailID}},
		"onSuccessUpdateEmail": map[string]any{"#s": map[string]any{
			"mailboxIds/" + j.box("drafts"): nil,
			"mailboxIds/" + j.box("sent"):   true,
			"keywords/$draft":               nil,
		}},
	}}
}

func (j *jmapBackend) Send(ctx context.Context, out *Outgoing) error {
	identity, err := j.identityID(ctx)
	if err != nil {
		return err
	}
	res, err := j.call(ctx,
		jcall{"Email/set", map[string]any{"create": map[string]any{"d": j.emailObject(out)}}},
		j.submission(identity, "#d"))
	if err != nil {
		return err
	}
	var made, sub setResult
	_ = json.Unmarshal(res[0], &made)
	_ = json.Unmarshal(res[1], &sub)
	if e, ok := made.NotCreated["d"]; ok {
		return &e
	}
	if e, ok := sub.NotCreated["s"]; ok {
		// Don't leave the message behind as a draft nobody asked for.
		if id := made.Created["d"].ID; id != "" {
			_, _ = j.call(ctx, jcall{"Email/set", map[string]any{"destroy": []string{id}}})
		}
		return &e
	}
	return nil
}

func (j *jmapBackend) SendDraft(ctx context.Context, id string, _ *Message) error {
	identity, err := j.identityID(ctx)
	if err != nil {
		return err
	}
	res, err := j.call(ctx, j.submission(identity, id))
	if err != nil {
		return err
	}
	var sub setResult
	_ = json.Unmarshal(res[0], &sub)
	if e, ok := sub.NotCreated["s"]; ok {
		return &e
	}
	return nil
}

func (j *jmapBackend) Discard(ctx context.Context, id string) error {
	var r struct {
		NotDestroyed map[string]jmapError `json:"notDestroyed"`
	}
	if err := j.call1(ctx, "Email/set", map[string]any{"destroy": []string{id}}, &r); err != nil {
		return err
	}
	if e, ok := r.NotDestroyed[id]; ok {
		return &e
	}
	return nil
}

func (j *jmapBackend) Archive(ctx context.Context, id string) error {
	box := j.box("archive")
	if box == "" {
		return errors.New("this account has no Archive mailbox")
	}
	return j.update(ctx, id, map[string]any{"mailboxIds": map[string]bool{box: true}})
}

// Watch listens on the EventSource when the server offers one, and polls
// when it doesn't or the stream keeps failing.
func (j *jmapBackend) Watch(ctx context.Context) error {
	j.mu.Lock()
	es := j.sess.EventSourceURL
	j.mu.Unlock()
	fails := 0
	for es != "" && fails < j.timing.pushFails {
		got, err := j.listen(ctx, es)
		if ctx.Err() != nil {
			return nil
		}
		if got {
			fails = 0
		} else {
			fails++
		}
		if err != nil {
			var je *jmapError
			if errors.As(err, &je) {
				return err // the API itself failed: reconnect
			}
		}
		if err := j.checkNew(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(min(j.timing.retryMin, 5*time.Second)):
		}
	}
	j.setPush(fmt.Sprintf("polling every %s", j.timing.poll))
	tick := time.NewTicker(j.timing.poll)
	defer tick.Stop()
	for {
		if err := j.checkNew(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (j *jmapBackend) setPush(s string) {
	j.mu.Lock()
	j.push = s
	j.mu.Unlock()
}

// listen reads one EventSource connection until it ends. got says whether
// it worked at all (so a stream that drops now and then isn't "failing").
func (j *jmapBackend) listen(ctx context.Context, tmpl string) (got bool, err error) {
	u := strings.NewReplacer("{types}", "Email", "{closeafter}", "no", "{ping}", "30").Replace(tmpl)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return false, err
	}
	j.auth(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := j.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return false, fmt.Errorf("EventSource: HTTP %d", resp.StatusCode)
	}
	j.setPush("EventSource")
	// Catch anything that arrived before the stream opened.
	if err := j.checkNew(ctx); err != nil {
		return true, err
	}
	// Pings come every 30 seconds; silence for much longer means the
	// connection is dead even if TCP hasn't noticed.
	watchdog := time.AfterFunc(2*time.Minute, cancel)
	defer watchdog.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	event, data := "", ""
	for sc.Scan() {
		watchdog.Reset(2 * time.Minute)
		line := sc.Text()
		switch {
		case line == "":
			if event == "state" || (event == "" && strings.Contains(data, "StateChange")) {
				if err := j.checkNew(ctx); err != nil {
					return true, err
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return true, sc.Err()
}

// checkNew asks what changed since the last state and reports new mail
// in the inbox (not drafts, not our own sent copies).
func (j *jmapBackend) checkNew(ctx context.Context) error {
	j.mu.Lock()
	since := j.state
	j.mu.Unlock()
	var created []string
	for {
		var r struct {
			NewState       string   `json:"newState"`
			HasMoreChanges bool     `json:"hasMoreChanges"`
			Created        []string `json:"created"`
		}
		err := j.call1(ctx, "Email/changes", map[string]any{"sinceState": since, "maxChanges": 256}, &r)
		var je *jmapError
		if errors.As(err, &je) && je.Type == "cannotCalculateChanges" {
			// The server forgot that far back: start again from now.
			st, err := j.emailState(ctx)
			if err != nil {
				return err
			}
			j.mu.Lock()
			j.state = st
			j.mu.Unlock()
			return nil
		}
		if err != nil {
			return err
		}
		created = append(created, r.Created...)
		if r.NewState == since || r.NewState == "" {
			break
		}
		since = r.NewState
		if !r.HasMoreChanges {
			break
		}
	}
	if len(created) > 0 {
		list, err := j.getEmails(ctx, created, fullProps)
		if err != nil {
			return err
		}
		inbox := j.box("inbox")
		for i := range list {
			e := &list[i]
			if e.MailboxIDs[inbox] && !e.Keywords["$draft"] {
				j.onNew(j.toMessage(e))
			}
		}
	}
	j.mu.Lock()
	j.state = since
	j.mu.Unlock()
	return nil
}

func (j *jmapBackend) Checks(ctx context.Context) []Check {
	sess, err := j.fetchSession(ctx)
	if err != nil {
		return []Check{{Name: "session", OK: false, Detail: err.Error()}}
	}
	checks := []Check{{Name: "session", OK: true, Detail: "JMAP at " + hostOf(sess.APIURL)}, {Name: "auth", OK: true}}
	j.mu.Lock()
	push := j.push
	j.mu.Unlock()
	if push == "" {
		push = "starting"
	}
	checks = append(checks, Check{Name: "push", OK: true, Detail: push})
	if _, ok := sess.Capabilities[jmapSubmission]; !ok {
		checks = append(checks, Check{Name: "smtp", OK: false, Detail: "the JMAP server doesn't offer sending (submission)"})
	} else if _, err := j.identityID(ctx); err != nil {
		checks = append(checks, Check{Name: "smtp", OK: false, Detail: err.Error()})
	} else {
		checks = append(checks, Check{Name: "smtp", OK: true, Detail: "JMAP submission as " + j.cfg.Address})
	}
	return checks
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (j *jmapBackend) Close() { j.hc.CloseIdleConnections() }
