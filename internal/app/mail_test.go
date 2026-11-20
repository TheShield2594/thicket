package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/dashboard"
	"github.com/theshield2594/thicket/internal/mail"
	"github.com/theshield2594/thicket/internal/store"
)

// fakeMail is the thicket-mail plugin as core sees it: MCP tools that
// follow the sending rules from what core says (_meta), and the admin
// API. It records what core sent it.
type fakeMail struct {
	mu       sync.Mutex
	calls    []mailCall // tools/call, in order
	sent     []string   // draft ids sent by a person
	discards []string
	accounts []mail.Account
	nextID   int
	// What the dashboard reads.
	box map[string][]mail.Summary
	msg map[string]*mail.Message
}

type mailCall struct {
	Tool string
	Args map[string]any
	Meta struct {
		Call struct {
			Sprig     string `json:"sprig"`
			Approved  bool   `json:"approved"`
			DraftOnly bool   `json:"draft_only"`
		} `json:"thicket/call"`
		Mail *struct {
			Allow []string `json:"allow"`
		} `json:"thicket/mail"`
	}
	HasMeta bool
}

func (f *fakeMail) last(t *testing.T) mailCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the mail plugin wasn't called")
	}
	return f.calls[len(f.calls)-1]
}

const mailToken = "mail-token-0123456789abcdefghijklmnopqrstuvwxyz"

func (f *fakeMail) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+mailToken {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /mcp", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments map[string]any  `json:"arguments"`
				Meta      json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		gates := `"_meta":{"thicket/gates_itself":true,"thicket/trailed":true}`
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"thicket-mail","version":"test"}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[`+
				`{"name":"list_mail","description":"list","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},`+
				`{"name":"send_mail","description":"send","inputSchema":{"type":"object"},%s},`+
				`{"name":"archive_mail","description":"archive","inputSchema":{"type":"object"},"_meta":{"thicket/trailed":true}}]}}`, *req.ID, gates)
		case "tools/call":
			c := mailCall{Tool: req.Params.Name, Args: req.Params.Arguments, HasMeta: len(req.Params.Meta) > 0}
			_ = json.Unmarshal(req.Params.Meta, &c.Meta)
			f.mu.Lock()
			f.calls = append(f.calls, c)
			f.nextID++
			id := fmt.Sprintf("d%d", f.nextID)
			f.mu.Unlock()
			res := map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
			if c.Tool == "send_mail" {
				var to []string
				for _, v := range c.Args["to"].([]any) {
					to = append(to, v.(string))
				}
				allowed := c.Meta.Mail != nil
				for _, r := range to {
					if allowed && !mail.Allowed(c.Meta.Mail.Allow, r) {
						allowed = false
					}
				}
				if c.Meta.Call.Approved || allowed && !c.Meta.Call.DraftOnly {
					res["content"] = []any{map[string]any{"type": "text", "text": "SENT"}}
				} else {
					res["content"] = []any{map[string]any{"type": "text", "text": "DRAFTED: waiting in needs you"}}
					res["_meta"] = map[string]any{"thicket/drafted": mail.Drafted{DraftID: id, From: "wren@example.org", To: to, Subject: "Hello <b>there</b>", Preview: "Body text", Reason: to[0] + " isn't on Wren's mail allowlist"}}
				}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": res})
			w.Write(b)
		}
	}))
	mux.HandleFunc("PUT /v1/accounts", auth(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Accounts []mail.Account `json:"accounts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.accounts = b.Accounts
		f.mu.Unlock()
		w.Write([]byte(`{"accounts":[]}`))
	}))
	mux.HandleFunc("GET /v1/events", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"epoch":"e1","next":0,"events":[]}`))
	}))
	mux.HandleFunc("POST /v1/mailbox/{sprig}/drafts/{id}/send", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sent = append(f.sent, r.PathValue("sprig")+"/"+r.PathValue("id"))
		f.mu.Unlock()
		w.Write([]byte(`{"message_id":"<m1@example.org>"}`))
	}))
	mux.HandleFunc("POST /v1/mailbox/{sprig}/drafts/{id}/discard", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.discards = append(f.discards, r.PathValue("sprig")+"/"+r.PathValue("id"))
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("GET /v1/mailbox/{sprig}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"messages": f.box[r.PathValue("sprig")]})
	}))
	mux.HandleFunc("GET /v1/mailbox/{sprig}/messages/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := f.msg[r.PathValue("sprig")+"/"+r.PathValue("id")]
		if m == nil {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such message"}`))
			return
		}
		json.NewEncoder(w).Encode(m)
	}))
	mux.HandleFunc("GET /v1/mailbox/{sprig}/messages/{id}/attachments/{aid}", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Mail-Filename", `../../evil"name.html`)
		w.Header().Set("X-Mail-Type", "text/html")
		w.Write([]byte("<script>alert(1)</script>"))
	}))
	return mux
}

const wrenMail = `---
id: wren
name: Wren
role: Handles signups and receipts.
personality:
  traits: [tidy]
voice:
  style: [short]
household: solo
belongs_to: 1
tools: ["mcp:mail/*"]
---
Wren keeps the household's paperwork.
`

const fernMail = `---
id: fern
name: Fern
role: The household's errand runner.
personality:
  traits: [quick]
voice:
  style: [short]
household: family
tools: ["mcp:mail/*"]
---
Fern runs errands.
`

// mailReady is a household with the mail plugin connected (as Thicket
// sets it up: the managed integration with the shared token), Wren (the
// owner's private sprig) and Fern (shared), both with an address.
func mailReady(t *testing.T) (*App, *fakeChannel, context.Context, *fakeMail, *store.User) {
	t.Helper()
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if owner.ID != 1 {
		t.Fatalf("owner is user %d", owner.ID)
	}
	for name, body := range map[string]string{"wren.md": wrenMail, "fern.md": fernMail} {
		if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	fm := &fakeMail{box: map[string][]mail.Summary{}, msg: map[string]*mail.Message{}}
	srv := httptest.NewServer(fm.handler(t))
	t.Cleanup(srv.Close)
	if err := a.Secrets.Put(ctx, "mail_token", mailToken); err != nil {
		t.Fatal(err)
	}
	s := store.MCPServer{Name: "mail", Transport: "http", Target: srv.URL + "/mcp", Enabled: true,
		Headers: map[string]string{"Authorization": "Bearer {{secret:mail_token}}"}}
	if err := a.DB.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MCP.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	cl := &mail.Client{URL: srv.URL, Token: func() (string, error) { return mailToken, nil }}
	a.Engine.Mail = cl
	a.Dashboard.Mail = &dashboard.Mailbox{Client: cl, Engine: a.Engine}
	for _, sp := range []string{"wren", "fern"} {
		if err := a.DB.PutMailAccount(ctx, store.MailAccount{Sprig: sp, Address: sp + "@example.org", Backend: "jmap",
			Settings: `{"session_url":"https://jmap.example.org/session","username":"` + sp + `@example.org"}`, SecretName: "mail_" + sp + "_password",
			RetentionDays: 90, SendLimit: 20, CreatedBy: owner.ID}); err != nil {
			t.Fatal(err)
		}
		_ = a.Secrets.Put(ctx, "mail_"+sp+"_password", "app-password-"+sp+"-123456")
	}
	fc.take()
	return a, fc, ctx, fm, owner
}

func sendCall(to string) string {
	return `wren, call tool mcp_mail__send_mail {"to":["` + to + `"],"subject":"Hi","body":"Hello"}`
}

func TestMailSendingRulesThroughCore(t *testing.T) {
	a, fc, ctx, fm, owner := mailReady(t)
	e := a.Engine
	if _, err := e.ChangeMailAllow(ctx, owner, "wren", []string{"Shop@Vendor.example", "@trusted.example"}, nil, "test"); err != nil {
		t.Fatal(err)
	}

	// An allowlisted vendor at the default level (a tool that gates
	// itself acts by default): sent, and core said who called and what's
	// allowed.
	e.HandleIncoming(ctx, dm(sendCall("shop@vendor.example")))
	fc.take()
	c := fm.last(t)
	if c.Meta.Call.Sprig != "wren" || c.Meta.Call.Approved || c.Meta.Call.DraftOnly || c.Meta.Mail == nil ||
		strings.Join(c.Meta.Mail.Allow, ",") != "@trusted.example,shop@vendor.example" {
		t.Fatalf("meta: %+v allow %+v", c.Meta.Call, c.Meta.Mail)
	}
	if items, _ := a.DB.OpenInbox(ctx, owner.ID); len(items) != 0 {
		t.Fatalf("a send to the allowlist left something in needs you: %+v", items)
	}
	// The plugin trails mail itself: core doesn't copy the arguments
	// (the body) into the trail.
	for _, tr := range mustTrail(t, a, ctx) {
		if strings.Contains(tr.Detail, "Hello") {
			t.Fatalf("a mail body reached the trail: %+v", tr)
		}
	}

	// Someone not on the list: drafted, and in "needs you" as an email
	// with Send and Discard.
	e.HandleIncoming(ctx, dm(sendCall("stranger@else.example")))
	fc.take()
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxDraft || !strings.Contains(items[0].Title, "Wren drafted an email to stranger@else.example") ||
		!strings.Contains(items[0].Detail, "isn't on Wren's mail allowlist") {
		t.Fatalf("needs you: %+v", items)
	}
	if err := e.RunDraft(ctx, owner, items[0].ID); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	sent := strings.Join(fm.sent, ",")
	fm.mu.Unlock()
	if sent != "wren/d2" {
		t.Fatalf("Send didn't send the draft through the admin API: %q", sent)
	}
	if open, _ := a.DB.OpenInbox(ctx, owner.ID); len(open) != 0 {
		t.Fatalf("sent draft still open: %+v", open)
	}

	// Another draft, discarded from "needs you": gone from the mailbox too.
	e.HandleIncoming(ctx, dm(sendCall("other@else.example")))
	fc.take()
	items, _ = a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 {
		t.Fatalf("needs you: %+v", items)
	}
	if err := e.DismissInbox(ctx, owner, items[0].ID); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	discarded := strings.Join(fm.discards, ",")
	fm.mu.Unlock()
	if discarded != "wren/d3" {
		t.Fatalf("Discard didn't discard: %q", discarded)
	}

	// At draft level the plugin is still called, told to only draft:
	// even an allowlisted address is drafted.
	a.DB.SetToolLevel(ctx, "wren", "mcp_mail__send_mail", store.LevelDraft)
	e.HandleIncoming(ctx, dm(sendCall("shop@vendor.example")))
	fc.take()
	if c := fm.last(t); !c.Meta.Call.DraftOnly || c.Meta.Call.Approved {
		t.Fatalf("draft level: %+v", c.Meta.Call)
	}

	// At ask it asks first; approved, the plugin is told a person said yes.
	a.DB.SetToolLevel(ctx, "wren", "mcp_mail__send_mail", store.LevelAsk)
	before := len(fm.calls)
	e.HandleIncoming(ctx, dm(sendCall("anyone@else.example")))
	if out := sentText(fc.take()); !strings.Contains(out, "I need your OK") || len(fm.calls) != before {
		t.Fatalf("ask didn't ask first:\n%s", out)
	}
	e.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if c := fm.last(t); !c.Meta.Call.Approved || c.Meta.Call.DraftOnly {
		t.Fatalf("approved call: %+v", c.Meta.Call)
	}

	// At read it's refused, and the plugin never hears of it.
	a.DB.SetToolLevel(ctx, "wren", "mcp_mail__send_mail", store.LevelRead)
	before = len(fm.calls)
	e.HandleIncoming(ctx, dm(sendCall("shop@vendor.example")))
	fc.take()
	if len(fm.calls) != before {
		t.Fatal("a read-level send reached the plugin")
	}

	// An integration called "mail" that Thicket didn't set up (no shared
	// token) gets no _meta, and its hints aren't honoured.
	var s store.MCPServer
	servers, _ := a.DB.MCPServers(ctx)
	for _, sv := range servers {
		if sv.Name == "mail" {
			s = sv
		}
	}
	s.Headers = map[string]string{"Authorization": "Bearer " + mailToken}
	a.DB.PutMCPServer(ctx, s)
	a.MCP.Connect(ctx, s)
	a.DB.SetToolLevel(ctx, "wren", "mcp_mail__send_mail", "")
	before = len(fm.calls)
	e.HandleIncoming(ctx, dm(sendCall("shop@vendor.example")))
	if out := sentText(fc.take()); !strings.Contains(out, "I need your OK") || len(fm.calls) != before {
		t.Fatalf("a hand-added mail integration was trusted to gate itself:\n%s", out)
	}
	e.HandleIncoming(ctx, dm("approve"))
	fc.take()
	if c := fm.last(t); c.HasMeta {
		t.Fatalf("a hand-added integration was told who's calling: %+v", c.Meta)
	}
}

func mustTrail(t *testing.T, a *App, ctx context.Context) []store.TrailEntry {
	t.Helper()
	tr, err := a.DB.Trail(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestMailAllowlistOwnership(t *testing.T) {
	a, _, ctx, _, owner := mailReady(t)
	e := a.Engine
	kim, err := a.DB.CreateUser(ctx, "discord", "kim", "Kim", "member")
	if err != nil {
		t.Fatal(err)
	}
	kid, err := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	if err != nil {
		t.Fatal(err)
	}
	// Entries are checked and normalised; wildcards aren't entries.
	for _, bad := range []string{"*@x.example", "not an address", "@-bad-.example"} {
		if _, err := e.ChangeMailAllow(ctx, owner, "fern", []string{bad}, nil, "test"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	list, err := e.ChangeMailAllow(ctx, owner, "fern", []string{" Shop@Vendor.Example ", "@Store.Example"}, nil, "test")
	if err != nil || strings.Join(list, ",") != "@store.example,shop@vendor.example" {
		t.Fatalf("%v %v", list, err)
	}
	// A shared sprig: a member can see the list but not change it; a kid
	// can do neither. Someone else's private sprig doesn't exist for them.
	if _, err := e.ChangeMailAllow(ctx, kim, "fern", []string{"me@kim.example"}, nil, "test"); err == nil {
		t.Fatal("a member changed a shared sprig's allowlist")
	}
	if l, err := e.MailAllowlist(ctx, kim, "fern"); err != nil || len(l) != 2 {
		t.Fatalf("a member can't see a shared sprig's allowlist: %v %v", l, err)
	}
	if _, err := e.MailAllowlist(ctx, kid, "fern"); err == nil {
		t.Fatal("a kid read a sprig's allowlist")
	}
	if _, err := e.MailAllowlist(ctx, kim, "wren"); err == nil {
		t.Fatal("someone read another person's private sprig's allowlist")
	}
	if _, err := e.ChangeMailAllow(ctx, kim, "wren", []string{"me@kim.example"}, nil, "test"); err == nil || strings.Contains(err.Error(), "Wren") {
		t.Fatalf("someone changed (or learned of) another person's private sprig: %v", err)
	}
	// Every change is in the trail, and undo puts it back.
	var change *store.TrailEntry
	for _, tr := range mustTrail(t, a, ctx) {
		if strings.Contains(tr.Summary, "Fern's mail allowlist: added") {
			tr := tr
			change = &tr
			break
		}
	}
	if change == nil || !change.Undoable {
		t.Fatalf("no undoable trail entry")
	}
	if _, err := e.UndoTrail(ctx, kim, change.ID); err == nil {
		t.Fatal("a member undid an allowlist change on a shared sprig")
	}
	if _, err := e.UndoTrail(ctx, owner, change.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := a.DB.MailAllow(ctx, "fern"); len(l) != 0 {
		t.Fatalf("undo left %v", l)
	}
}

func TestMailKidSprigsHaveNoMail(t *testing.T) {
	a, fc, ctx, _, owner := mailReady(t)
	kid, _ := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	body := fmt.Sprintf("---\nid: moss\nname: Moss\nrole: A reading buddy.\npersonality:\n  traits: [kind]\nvoice:\n  style: [gentle]\nhousehold: kid\nkids: [%d]\nparents: [%d]\n---\nMoss reads.\n", kid.ID, owner.ID)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "moss.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, dm(`call tool mail_address {"sprig":"moss","action":"set","address":"moss@example.org","backend":"jmap","session_url":"https://jmap.example.org/session"}`))
	fc.take()
	if _, err := a.DB.MailAccount(ctx, "moss"); err == nil {
		t.Fatal("a kid sprig got an email address")
	}
	if _, err := a.Engine.ChangeMailAllow(ctx, owner, "moss", []string{"a@b.example"}, nil, "test"); err == nil {
		t.Fatal("a kid sprig got a mail allowlist")
	}
	a.Engine.HandleIncoming(ctx, dm(`call tool grant_tools {"character":"moss","add":["mcp:mail/*"]}`))
	fc.take()
	if c, _ := a.Chars.Get("moss"); len(c.Tools) != 0 {
		t.Fatalf("a kid sprig was granted the mail plugin: %v", c.Tools)
	}
}

func TestMailAddressSetupInChat(t *testing.T) {
	a, fc, ctx, fm, _ := mailReady(t)
	a.Engine.HandleIncoming(ctx, dm(`call tool mail_address {"sprig":"fern","action":"set","address":"Fern@Errands.example","backend":"imap","imap_host":"imap.errands.example","smtp_host":"smtp.errands.example","smtp_security":"starttls"}`))
	out := sentText(fc.take())
	acct, err := a.DB.MailAccount(ctx, "fern")
	if err != nil || acct.Address != "fern@errands.example" || acct.Backend != "imap" {
		t.Fatalf("%+v %v\n%s", acct, err, out)
	}
	var s mail.Settings
	json.Unmarshal([]byte(acct.Settings), &s)
	if s.IMAPPort != 993 || s.SMTPPort != 587 || s.Username != "fern@errands.example" || strings.Contains(acct.Settings, "password") {
		t.Fatalf("settings: %s", acct.Settings)
	}
	// Plaintext is refused.
	a.Engine.HandleIncoming(ctx, dm(`call tool mail_address {"sprig":"fern","action":"set","address":"fern@errands.example","backend":"imap","imap_host":"imap.errands.example","smtp_host":"smtp.errands.example","imap_security":"none"}`))
	fc.take()
	if acct2, _ := a.DB.MailAccount(ctx, "fern"); acct2.Settings != acct.Settings {
		t.Fatal("a plaintext setup was saved")
	}
	// The password arrives as the next DM: captured, never stored in the
	// chat, and pushed to the plugin (in memory there).
	a.DB.SetPendingSecret(ctx, store.PendingSecret{UserID: 1, Name: "mail_fern_password", Purpose: "app password"})
	a.Engine.HandleIncoming(ctx, dm("xq7-app-pass-word-991"))
	fc.take()
	if v, _ := a.Secrets.Get(ctx, "mail_fern_password"); v != "xq7-app-pass-word-991" {
		t.Fatalf("password not stored: %q", v)
	}
	accts, err := a.Engine.MailAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var fern *mail.Account
	for i := range accts {
		if accts[i].Sprig == "fern" {
			fern = &accts[i]
		}
	}
	if fern == nil || fern.Password != "xq7-app-pass-word-991" || fern.IMAP == nil || fern.IMAP.Security != "tls" || fern.SMTP.Security != "starttls" {
		t.Fatalf("account for the plugin: %+v", fern)
	}
	if _, err := a.Engine.Mail.PutAccounts(ctx, accts); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	n := len(fm.accounts)
	fm.mu.Unlock()
	if n != 2 {
		t.Fatalf("plugin got %d accounts", n)
	}
	if strings.Contains(a.Secrets.Redact("pw xq7-app-pass-word-991"), "xq7") {
		t.Fatal("the mail password isn't redacted")
	}
}

func TestMailEventsTrailAndCodes(t *testing.T) {
	a, _, ctx, _, owner := mailReady(t)
	a.Engine.MailEvent(ctx, mail.Event{Seq: 1, Sprig: "wren", Kind: "received", At: time.Now(), From: "Vendor <no-reply@vendor.example>",
		To: []string{"wren@example.org"}, Subject: "Your code is 482913", Size: 2048, MessageID: "<abc@vendor.example>", OTP: true})
	a.Engine.MailEvent(ctx, mail.Event{Seq: 2, Sprig: "wren", Kind: "sent", At: time.Now(), From: "wren@example.org",
		To: []string{"shop@vendor.example"}, Cc: []string{"x@y.example"}, Subject: "Order", Size: 512, MessageID: "<def@example.org>"})
	var got []string
	for _, tr := range mustTrail(t, a, ctx) {
		if tr.Kind == store.TrailMail {
			got = append(got, tr.Summary+" | "+tr.Detail)
		}
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"Wren received mail from Vendor <no-reply@vendor.example>", "message id: <abc@vendor.example>", "size: 2048 bytes",
		"Wren sent mail to shop@vendor.example: Order", "cc: x@y.example"} {
		if !strings.Contains(all, want) {
			t.Errorf("trail lacks %q:\n%s", want, all)
		}
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	if len(items) != 1 || items[0].Kind != store.InboxCode || !strings.Contains(items[0].Title, "one-time code arrived for Wren") {
		t.Fatalf("needs you: %+v", items)
	}
}

func TestMailboxOnTheDashboard(t *testing.T) {
	a, _, ctx, fm, owner := mailReady(t)
	kim, _ := a.DB.CreateUser(ctx, "discord", "kim", "Kim", "admin")
	kid, _ := a.DB.CreateUser(ctx, "discord", "kid", "Ash", "kid")
	hostile := `<script>alert("x")</script><img src=https://evil.example/p.png onerror=alert(1)>`
	fm.box["wren"] = []mail.Summary{{ID: "m1", From: `"<b>Eve</b>" <eve@evil.example>`, Subject: hostile, Date: "2026-11-20 09:00", Preview: hostile, Attachments: 1}}
	fm.box["fern"] = []mail.Summary{{ID: "f1", From: "shop@vendor.example", Subject: "Receipt", Draft: true}}
	fm.msg["wren/m1"] = &mail.Message{ID: "m1", From: `"<b>Eve</b>" <eve@evil.example>`, To: []string{"wren@example.org"}, Subject: hostile,
		Text: "Click here <https://evil.example/login>\n" + hostile + `<style>body{display:none}</style>`,
		Attachments: []mail.Attachment{{ID: "a1", Name: "invoice<script>.pdf", Type: "application/pdf", Size: 2048}}}
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session := func(u *store.User) string {
		s, err := a.DB.CreateWebSession(ctx, u.ID, "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	get := func(sess, path string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: sess})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	own, admin, child := session(owner), session(kim), session(kid)

	// The owner reads their private sprig's mail: every hostile byte is
	// text, links show their real target.
	code, body, hdr := get(own, "/dashboard/mail?sprig=wren")
	if code != 200 || strings.Contains(body, "<script>alert") || strings.Contains(body, "<img") || strings.Contains(body, "<b>Eve") {
		t.Fatalf("list %d: injection got through:\n%s", code, body)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP: %q", hdr.Get("Content-Security-Policy"))
	}
	code, body, _ = get(own, "/dashboard/mail/message?sprig=wren&id=m1")
	if code != 200 || strings.Contains(body, "<script>") || strings.Contains(body, "<style>body") || strings.Contains(body, "<img") ||
		!strings.Contains(body, "Click here &lt;https://evil.example/login&gt;") || !strings.Contains(body, "invoice&lt;script&gt;.pdf") || !strings.Contains(body, "2 KB") {
		t.Fatalf("message %d:\n%s", code, body)
	}
	// Attachments only ever download, as a file with a safe name.
	code, _, hdr = get(own, "/dashboard/mail/attachment?sprig=wren&id=m1&aid=a1")
	if code != 200 || hdr.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment;") ||
		strings.Contains(hdr.Get("Content-Disposition"), "/") || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("attachment %d: %v", code, hdr)
	}

	// An admin doesn't see someone else's private sprig's mail, not even
	// that it has a mailbox; they do see a shared sprig's.
	if code, body, _ := get(admin, "/dashboard/mail?sprig=wren"); code != 404 || strings.Contains(body, "wren@example.org") {
		t.Fatalf("admin opened a private sprig's mail: %d", code)
	}
	if code, _, _ := get(admin, "/dashboard/mail/message?sprig=wren&id=m1"); code != 404 {
		t.Fatalf("admin read a private sprig's message: %d", code)
	}
	if code, body, _ := get(admin, "/dashboard/mail"); code != 200 || strings.Contains(body, "wren@example.org") || !strings.Contains(body, "fern@example.org") {
		t.Fatalf("admin's mailboxes %d:\n%s", code, body)
	}
	// Kids see no mailbox at all.
	if code, _, _ := get(child, "/dashboard/mail?sprig=fern"); code != 404 {
		t.Fatalf("a kid opened a mailbox: %d", code)
	}

	// Sending a draft from the mailbox: CSRF-checked, then sent.
	post := func(sess string, form url.Values) int {
		req, _ := http.NewRequest("POST", srv.URL+"/dashboard/mail/act", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: dashboard.SessionCookie, Value: sess})
		cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(admin, url.Values{"sprig": {"fern"}, "id": {"f1"}, "action": {"send"}, "csrf": {"wrong"}}); code != 403 {
		t.Fatalf("no CSRF check: %d", code)
	}
	if code := post(admin, url.Values{"sprig": {"fern"}, "id": {"f1"}, "action": {"send"}, "csrf": {csrfFor(admin)}}); code != 303 {
		t.Fatalf("send: %d", code)
	}
	if code := post(admin, url.Values{"sprig": {"wren"}, "id": {"m1"}, "action": {"send"}, "csrf": {csrfFor(admin)}}); code != 404 {
		t.Fatalf("admin sent from a private sprig's mailbox: %d", code)
	}
	fm.mu.Lock()
	sent := strings.Join(fm.sent, ",")
	fm.mu.Unlock()
	if sent != "fern/f1" {
		t.Fatalf("sent: %q", sent)
	}
	// The allowlist: its owner (an admin, for a shared sprig) changes it here.
	if code := post(admin, url.Values{"sprig": {"fern"}, "entry": {"@vendor.example"}, "action": {"allow"}, "csrf": {csrfFor(admin)}}); code != 303 {
		t.Fatalf("allow: %d", code)
	}
	if l, _ := a.DB.MailAllow(ctx, "fern"); strings.Join(l, ",") != "@vendor.example" {
		t.Fatalf("allowlist: %v", l)
	}
	if code, body, _ := get(own, "/dashboard"); code != 200 || !strings.Contains(body, `href="/dashboard/mail"`) {
		t.Fatalf("no Mail link on the dashboard")
	}
}

// csrfFor is the dashboard's form token for a session.
func csrfFor(session string) string { return dashboard.CSRFToken(session) }
