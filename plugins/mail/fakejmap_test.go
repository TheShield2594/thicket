package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJMAP is a small JMAP server: just what the plugin uses, over TLS.
type fakeJMAP struct {
	t          *testing.T
	srv        *httptest.Server
	user, pass string
	address    string
	offerES    bool

	mu          sync.Mutex
	emails      map[string]*jEmail
	blobs       map[string][]byte
	state       int
	log         []fakeChange
	nextID      int
	submissions []fakeSent
	subs        map[chan struct{}]bool
	born        map[string]int // id -> state it was created in
}

type fakeChange struct {
	state int
	id    string
}

// fakeSent is one message the fake "sent".
type fakeSent struct {
	rcpts   []string
	subject string
	body    string
	inReply string
}

var fakeBoxes = []map[string]string{
	{"id": "mb-inbox", "name": "Inbox", "role": "inbox"},
	{"id": "mb-drafts", "name": "Drafts", "role": "drafts"},
	{"id": "mb-sent", "name": "Sent", "role": "sent"},
	{"id": "mb-archive", "name": "Archive", "role": "archive"},
	{"id": "mb-trash", "name": "Trash", "role": "trash"},
}

func newFakeJMAP(t *testing.T, cert tls.Certificate, address string, offerES bool) *fakeJMAP {
	f := &fakeJMAP{t: t, user: "pip-user", pass: "s3cret-app-password", address: address, offerES: offerES,
		emails: map[string]*jEmail{}, blobs: map[string][]byte{}, subs: map[chan struct{}]bool{}, born: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/session", f.authed(f.session))
	mux.HandleFunc("POST /jmap/api", f.authed(f.api))
	mux.HandleFunc("GET /jmap/es", f.authed(f.eventSource))
	mux.HandleFunc("GET /jmap/download/{acct}/{blob}/{name}", f.authed(f.download))
	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	f.srv.StartTLS()
	t.Cleanup(func() {
		f.mu.Lock()
		for ch := range f.subs {
			close(ch)
		}
		f.subs = map[chan struct{}]bool{}
		f.mu.Unlock()
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *fakeJMAP) account() AccountConfig {
	return AccountConfig{Sprig: "pip", Address: f.address, Name: "Pip", Backend: "jmap",
		JMAP: jmapConfig{SessionURL: f.srv.URL + "/jmap/session"}, Username: f.user, Password: f.pass}
}

func (f *fakeJMAP) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			http.Error(w, "unauthorized", 401)
			return
		}
		h(w, r)
	}
}

func (f *fakeJMAP) session(w http.ResponseWriter, _ *http.Request) {
	s := map[string]any{
		"capabilities":    map[string]any{jmapCore: map[string]any{}, jmapMail: map[string]any{}, jmapSubmission: map[string]any{}},
		"primaryAccounts": map[string]string{jmapMail: "acct1", jmapSubmission: "acct1"},
		"apiUrl":          f.srv.URL + "/jmap/api",
		"downloadUrl":     f.srv.URL + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
	}
	if f.offerES {
		s["eventSourceUrl"] = f.srv.URL + "/jmap/es?types={types}&closeafter={closeafter}&ping={ping}"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

// bump records a change and wakes EventSource listeners (mu held).
func (f *fakeJMAP) bump(id string) {
	f.state++
	f.log = append(f.log, fakeChange{f.state, id})
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (f *fakeJMAP) newID() string {
	f.nextID++
	return "e" + strconv.Itoa(f.nextID)
}

// add puts a message into a mailbox, as if it arrived.
func (f *fakeJMAP) add(box string, e *jEmail) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = f.newID()
	e.MailboxIDs = map[string]bool{box: true}
	if e.Keywords == nil {
		e.Keywords = map[string]bool{}
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	e.MessageID = []string{e.ID + "@vendor.example"}
	for i := range e.Attach {
		f.blobs[e.Attach[i].BlobID] = []byte("blob " + e.Attach[i].BlobID)
	}
	f.emails[e.ID] = e
	f.bump(e.ID)
	f.born[e.ID] = f.state
	return e.ID
}

// deliver is incoming mail for the scenario.
func (f *fakeJMAP) deliver(from, subject, text, html string, at time.Time) {
	e := &jEmail{From: []jAddr{{Name: nameOf(from), Email: addrOf(from)}}, To: []jAddr{{Email: f.address}},
		Subject: subject, ReceivedAt: at, BodyValues: map[string]struct {
			Value       string `json:"value"`
			IsTruncated bool   `json:"isTruncated"`
		}{}}
	if text != "" {
		e.BodyValues["1"] = struct {
			Value       string `json:"value"`
			IsTruncated bool   `json:"isTruncated"`
		}{Value: text}
		e.TextBody = []jPart{{PartID: "1", Type: "text/plain"}}
		e.HTMLBody = e.TextBody
	}
	if html != "" {
		e.BodyValues["2"] = struct {
			Value       string `json:"value"`
			IsTruncated bool   `json:"isTruncated"`
		}{Value: html}
		e.HTMLBody = []jPart{{PartID: "2", Type: "text/html"}}
		if text == "" {
			e.TextBody = e.HTMLBody
		}
	}
	e.Size = int64(len(text) + len(html))
	f.add("mb-inbox", e)
}

func (f *fakeJMAP) sentMail() []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.submissions)
}

func nameOf(s string) string {
	if i := strings.Index(s, "<"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return ""
}

func addrOf(s string) string {
	if i := strings.Index(s, "<"); i >= 0 {
		return strings.TrimSuffix(s[i+1:], ">")
	}
	return s
}

func (f *fakeJMAP) eventSource(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("types") != "Email" {
		http.Error(w, "bad types", 400)
		return
	}
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs[ch] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	fmt.Fprint(w, "event: ping\ndata: {\"interval\":30}\n\n")
	fl.Flush()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			f.mu.Lock()
			st := f.state
			f.mu.Unlock()
			fmt.Fprintf(w, "event: state\ndata: {\"@type\":\"StateChange\",\"changed\":{\"acct1\":{\"Email\":\"%d\"}}}\n\n", st)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeJMAP) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	b, ok := f.blobs[r.PathValue("blob")]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

func (f *fakeJMAP) api(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Using       []string            `json:"using"`
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	created := map[string]string{} // creation id -> id, across the request
	var out [][]any
	for _, mc := range req.MethodCalls {
		var name, tag string
		var args map[string]json.RawMessage
		_ = json.Unmarshal(mc[0], &name)
		_ = json.Unmarshal(mc[1], &args)
		_ = json.Unmarshal(mc[2], &tag)
		res, errType := f.method(name, args, created)
		if errType != "" {
			out = append(out, []any{"error", map[string]string{"type": errType}, tag})
		} else {
			out = append(out, []any{name, res, tag})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": out, "sessionState": "1"})
}

func get[T any](args map[string]json.RawMessage, key string) T {
	var v T
	if raw, ok := args[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// method runs one JMAP method (mu held).
func (f *fakeJMAP) method(name string, args map[string]json.RawMessage, created map[string]string) (any, string) {
	if get[string](args, "accountId") != "acct1" {
		return nil, "accountNotFound"
	}
	switch name {
	case "Mailbox/get":
		return map[string]any{"accountId": "acct1", "state": "m1", "list": fakeBoxes}, ""
	case "Email/query":
		filter := get[map[string]string](args, "filter")
		var after time.Time
		if a := filter["after"]; a != "" {
			after, _ = time.Parse(time.RFC3339, a)
		}
		var list []*jEmail
		for _, e := range f.emails {
			if !e.MailboxIDs[filter["inMailbox"]] {
				continue
			}
			if k := filter["notKeyword"]; k != "" && e.Keywords[k] {
				continue
			}
			if !after.IsZero() && e.ReceivedAt.Before(after) {
				continue
			}
			list = append(list, e)
		}
		slices.SortFunc(list, func(a, b *jEmail) int { return b.ReceivedAt.Compare(a.ReceivedAt) })
		ids := []string{}
		for i, e := range list {
			if i < get[int](args, "limit") {
				ids = append(ids, e.ID)
			}
		}
		return map[string]any{"ids": ids, "queryState": "q", "position": 0}, ""
	case "Email/get":
		ids := get[[]string](args, "ids")
		list := []*jEmail{}
		notFound := []string{}
		for _, id := range ids {
			if e, ok := f.emails[id]; ok {
				list = append(list, e)
			} else {
				notFound = append(notFound, id)
			}
		}
		return map[string]any{"state": strconv.Itoa(f.state), "list": list, "notFound": notFound}, ""
	case "Email/changes":
		since, err := strconv.Atoi(get[string](args, "sinceState"))
		if err != nil || since > f.state {
			return nil, "cannotCalculateChanges"
		}
		createdIDs := []string{}
		for _, c := range f.log {
			if c.state > since && !slices.Contains(createdIDs, c.id) {
				createdIDs = append(createdIDs, c.id)
			}
		}
		// Treat every changed id as created if new since then; good enough.
		var fresh []string
		for _, id := range createdIDs {
			if e, ok := f.emails[id]; ok && e != nil && f.born[id] > since {
				fresh = append(fresh, id)
			}
		}
		return map[string]any{"oldState": strconv.Itoa(since), "newState": strconv.Itoa(f.state), "hasMoreChanges": false,
			"created": nonNil(fresh), "updated": []string{}, "destroyed": []string{}}, ""
	case "Email/set":
		res := map[string]any{"oldState": strconv.Itoa(f.state)}
		made := map[string]any{}
		notMade := map[string]any{}
		for cid, obj := range get[map[string]json.RawMessage](args, "create") {
			var e jEmail
			_ = json.Unmarshal(obj, &e)
			if len(e.MailboxIDs) == 0 {
				notMade[cid] = map[string]string{"type": "invalidProperties"}
				continue
			}
			e.ID = f.newID()
			e.ReceivedAt = time.Now().UTC()
			for _, v := range e.BodyValues {
				e.Size += int64(len(v.Value))
			}
			f.emails[e.ID] = &e
			f.bump(e.ID)
			f.born[e.ID] = f.state
			created["#"+cid] = e.ID
			made[cid] = map[string]string{"id": e.ID}
		}
		updated := map[string]any{}
		for id, patch := range get[map[string]map[string]json.RawMessage](args, "update") {
			e, ok := f.emails[id]
			if !ok {
				continue
			}
			f.patch(e, patch)
			updated[id] = nil
		}
		destroyed := []string{}
		for _, id := range get[[]string](args, "destroy") {
			if _, ok := f.emails[id]; ok {
				delete(f.emails, id)
				f.bump(id)
				destroyed = append(destroyed, id)
			}
		}
		res["created"], res["notCreated"], res["updated"], res["destroyed"] = made, notMade, updated, destroyed
		res["newState"] = strconv.Itoa(f.state)
		return res, ""
	case "Identity/get":
		return map[string]any{"list": []map[string]string{{"id": "ident1", "email": f.address, "name": "Pip"}}}, ""
	case "EmailSubmission/set":
		made := map[string]any{}
		notMade := map[string]any{}
		for cid, obj := range get[map[string]map[string]string](args, "create") {
			id := obj["emailId"]
			if strings.HasPrefix(id, "#") {
				id = created[id]
			}
			e, ok := f.emails[id]
			if !ok || obj["identityId"] != "ident1" {
				notMade[cid] = map[string]string{"type": "invalidProperties"}
				continue
			}
			var rcpts []string
			for _, l := range [][]jAddr{e.To, e.Cc, e.Bcc} {
				for _, a := range l {
					rcpts = append(rcpts, a.Email)
				}
			}
			body := ""
			for _, p := range e.TextBody {
				body += e.BodyValues[p.PartID].Value
			}
			s := fakeSent{rcpts: rcpts, subject: e.Subject, body: body}
			if len(e.InReplyTo) > 0 {
				s.inReply = e.InReplyTo[0]
			}
			f.submissions = append(f.submissions, s)
			made[cid] = map[string]string{"id": "sub" + strconv.Itoa(len(f.submissions))}
			if patch, ok := get[map[string]map[string]json.RawMessage](args, "onSuccessUpdateEmail")["#"+cid]; ok {
				f.patch(e, patch)
			}
		}
		return map[string]any{"created": made, "notCreated": notMade}, ""
	}
	return nil, "unknownMethod"
}

// patch applies a JMAP PatchObject (mu held).
func (f *fakeJMAP) patch(e *jEmail, p map[string]json.RawMessage) {
	for path, raw := range p {
		var on any
		_ = json.Unmarshal(raw, &on)
		switch {
		case path == "mailboxIds":
			e.MailboxIDs = map[string]bool{}
			_ = json.Unmarshal(raw, &e.MailboxIDs)
		case strings.HasPrefix(path, "mailboxIds/"):
			if on == nil {
				delete(e.MailboxIDs, strings.TrimPrefix(path, "mailboxIds/"))
			} else {
				e.MailboxIDs[strings.TrimPrefix(path, "mailboxIds/")] = true
			}
		case strings.HasPrefix(path, "keywords/"):
			if e.Keywords == nil {
				e.Keywords = map[string]bool{}
			}
			if on == nil {
				delete(e.Keywords, strings.TrimPrefix(path, "keywords/"))
			} else {
				e.Keywords[strings.TrimPrefix(path, "keywords/")] = true
			}
		}
	}
	f.bump(e.ID)
}
