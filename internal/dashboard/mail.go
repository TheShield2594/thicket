package dashboard

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/theshield2594/thicket/internal/mail"
	"github.com/theshield2594/thicket/internal/store"
)

// The Mail page: each sprig's mailbox (docs/mail.md, docs/dashboard.md).
//
//   - Who sees it: a private sprig's mailbox only for the person it
//     belongs to (admins don't see it); a shared sprig's for adults, not
//     kids. Kid sprigs have no mail. The engine decides (MailEngine).
//   - What it shows: text only. The plugin turns HTML into text on the
//     server, and this page escapes every byte of it: no markup, no
//     remote images, links written out with their real target.
//     Attachments are listed with their type and size and only download
//     when clicked, as a file (never shown in the page).
//   - What you can do: send or discard a draft (as in "needs you"), and,
//     if the sprig is yours to manage, change its allowlist.

// MailEngine is what the Mail page asks of the engine.
type MailEngine interface {
	CanReadMail(u *store.User, sprig string) bool
	CanManageMail(u *store.User, sprig string) bool
	MailAllowlist(ctx context.Context, u *store.User, sprig string) ([]string, error)
	ChangeMailAllow(ctx context.Context, by *store.User, sprig string, add, remove []string, via string) ([]string, error)
	SendMailDraft(ctx context.Context, u *store.User, sprig, id string) error
	DiscardMailDraft(ctx context.Context, u *store.User, sprig, id string) error
}

// Mailbox is the Mail page's plugin and engine.
type Mailbox struct {
	Client *mail.Client
	Engine MailEngine
}

type mailboxRef struct {
	ID, Name, Address string
}

// mailboxesFor lists the sprig addresses u may open.
func (d *Dashboard) mailboxesFor(ctx context.Context, u *store.User) []mailboxRef {
	if d.Mail == nil || u.IsKid() {
		return nil
	}
	accts, err := d.DB.MailAccounts(ctx)
	if err != nil {
		return nil
	}
	var out []mailboxRef
	for _, a := range accts {
		c, ok := d.Chars.Get(a.Sprig)
		if !ok || !d.Mail.Engine.CanReadMail(u, a.Sprig) {
			continue
		}
		out = append(out, mailboxRef{ID: a.Sprig, Name: c.DisplayName(), Address: a.Address})
	}
	return out
}

type mailPage struct {
	CSRF, Flash, Problem string
	Boxes                []mailboxRef
	Box                  *mailboxRef
	Folder               string
	Messages             []mail.Summary
	Message              *mail.Message
	Allow                []string
	CanManage            bool
}

// mailUser checks the session and, if a sprig is named, that u may open
// its mailbox. It writes the refusal itself.
func (d *Dashboard) mailUser(w http.ResponseWriter, r *http.Request) (*store.User, *mailPage, bool) {
	u, err := d.user(r)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, nil)
		return nil, nil, false
	}
	if d.Mail == nil {
		http.Error(w, "the mail plugin isn't running (docker compose --profile mail up -d)", http.StatusNotFound)
		return nil, nil, false
	}
	c, _ := r.Cookie(SessionCookie)
	p := &mailPage{CSRF: csrfToken(c.Value), Boxes: d.mailboxesFor(r.Context(), u)}
	if sprig := r.URL.Query().Get("sprig"); sprig != "" || r.Method == http.MethodPost {
		if sprig == "" {
			sprig = r.PostFormValue("sprig")
		}
		for i := range p.Boxes {
			if p.Boxes[i].ID == sprig {
				p.Box = &p.Boxes[i]
			}
		}
		if p.Box == nil { // not theirs, or no such mailbox: the same answer
			http.Error(w, "no mailbox you can open by that name", http.StatusNotFound)
			return nil, nil, false
		}
		p.CanManage = d.Mail.Engine.CanManageMail(u, sprig)
	} else if len(p.Boxes) > 0 {
		p.Box = &p.Boxes[0]
		p.CanManage = d.Mail.Engine.CanManageMail(u, p.Box.ID)
	}
	return u, p, true
}

func (d *Dashboard) mailbox(w http.ResponseWriter, r *http.Request) {
	u, p, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	p.Folder = "inbox"
	if r.URL.Query().Get("folder") == "drafts" {
		p.Folder = "drafts"
	}
	p.Flash = mailFlash(r.URL.Query().Get("ok"))
	code := http.StatusOK
	if p.Box != nil {
		var err error
		if p.Messages, err = d.Mail.Client.Mailbox(r.Context(), p.Box.ID, p.Folder, 50); err != nil {
			p.Problem, code = "Couldn't read the mailbox: "+err.Error(), http.StatusBadGateway
		}
		if p.Allow, err = d.Mail.Engine.MailAllowlist(r.Context(), u, p.Box.ID); err != nil && p.Problem == "" {
			p.Problem = "Couldn't read the allowlist: " + err.Error()
		}
	}
	d.mailRender(w, r, code, mailListTmpl, p)
}

func (d *Dashboard) mailMessage(w http.ResponseWriter, r *http.Request) {
	_, p, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	if p.Box == nil {
		http.Error(w, "name a mailbox", http.StatusBadRequest)
		return
	}
	m, err := d.Mail.Client.Message(r.Context(), p.Box.ID, r.URL.Query().Get("id"))
	if errors.Is(err, mail.ErrNotFound) {
		http.Error(w, "no such message", http.StatusNotFound)
		return
	}
	if err != nil {
		p.Problem = "Couldn't read the message: " + err.Error()
		d.mailRender(w, r, http.StatusBadGateway, mailMsgTmpl, p)
		return
	}
	p.Message = m
	d.mailRender(w, r, http.StatusOK, mailMsgTmpl, p)
}

// mailAttachment downloads one attachment, only ever as a file: never
// shown in the page, never sniffed, sandboxed if a browser tried.
func (d *Dashboard) mailAttachment(w http.ResponseWriter, r *http.Request) {
	_, p, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	if p.Box == nil {
		http.Error(w, "name a mailbox", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	body, hdr, err := d.Mail.Client.Attachment(r.Context(), p.Box.ID, q.Get("id"), q.Get("aid"))
	if errors.Is(err, mail.ErrNotFound) {
		http.Error(w, "no such attachment", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "couldn't fetch the attachment", http.StatusBadGateway)
		return
	}
	defer body.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(hdr.Get("X-Mail-Filename"))}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if n := hdr.Get("Content-Length"); n != "" {
		h.Set("Content-Length", n)
	}
	_, _ = io.Copy(w, io.LimitReader(body, 64<<20))
}

// safeFilename keeps a download's name to plain printable characters.
func safeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == '"' || !unicode.IsPrint(r):
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	s = strings.TrimLeft(s, ".")
	if len(s) > 120 {
		s = s[:120]
	}
	if s == "" {
		s = "attachment"
	}
	return s
}

// mailAct sends or discards a draft, or changes the allowlist.
func (d *Dashboard) mailAct(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	u, p, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	if !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(p.CSRF)) {
		http.Error(w, "this form has expired; reload the page", http.StatusForbidden)
		return
	}
	ctx, sprig, id := r.Context(), p.Box.ID, r.PostFormValue("id")
	var err error
	done, folder := "", "inbox"
	switch r.PostFormValue("action") {
	case "send":
		err, done, folder = d.Mail.Engine.SendMailDraft(ctx, u, sprig, id), "sent", "drafts"
	case "discard":
		err, done, folder = d.Mail.Engine.DiscardMailDraft(ctx, u, sprig, id), "discarded", "drafts"
	case "allow":
		_, err = d.Mail.Engine.ChangeMailAllow(ctx, u, sprig, []string{r.PostFormValue("entry")}, nil, "from the dashboard")
		done = "allowed"
	case "disallow":
		_, err = d.Mail.Engine.ChangeMailAllow(ctx, u, sprig, nil, []string{r.PostFormValue("entry")}, "from the dashboard")
		done = "disallowed"
	default:
		err = errors.New("unknown action")
	}
	if err != nil {
		p.Problem = "Couldn't do that: " + err.Error()
		p.Folder = folder
		p.Messages, _ = d.Mail.Client.Mailbox(ctx, sprig, folder, 50)
		p.Allow, _ = d.Mail.Engine.MailAllowlist(ctx, u, sprig)
		d.mailRender(w, r, http.StatusBadRequest, mailListTmpl, p)
		return
	}
	v := url.Values{"sprig": {sprig}, "folder": {folder}, "ok": {done}}
	http.Redirect(w, r, "/dashboard/mail?"+v.Encode(), http.StatusSeeOther)
}

func mailFlash(code string) string {
	switch code {
	case "sent":
		return "Sent."
	case "discarded":
		return "Discarded."
	case "allowed":
		return "Added to the allowlist. It's in the trail, and Undo there takes it back off."
	case "disallowed":
		return "Taken off the allowlist."
	}
	return ""
}

func (d *Dashboard) mailRender(w http.ResponseWriter, r *http.Request, code int, t *template.Template, p *mailPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := t.Execute(w, p); err != nil {
		d.Log.Error("mail page", "err", err)
	}
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

var mailFuncs = template.FuncMap{"size": size, "join": func(s []string) string { return strings.Join(s, ", ") }}

const mailHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Mail · Thicket</title>` + style + `</head>
<body><main>
<header><h1>✉️ Mail</h1><a href="/dashboard">← Dashboard</a></header>
{{if .Boxes}}<nav>{{range .Boxes}}<a href="/dashboard/mail?sprig={{.ID}}">{{.Name}} <span class="muted small">{{.Address}}</span></a>{{end}}</nav>{{end}}
{{if .Flash}}<section class="flash" role="status">{{.Flash}}</section>{{end}}
{{if .Problem}}<section class="problem" role="alert">{{.Problem}}</section>{{end}}`

const draftButtons = `{{define "draftbtns"}}<form class="inline" method="post" action="/dashboard/mail/act"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="sprig" value="{{.Sprig}}"><input type="hidden" name="id" value="{{.ID}}"><button class="primary" name="action" value="send">Send</button> <button name="action" value="discard">Discard</button></form>{{end}}`

type draftBtnArgs struct{ CSRF, Sprig, ID string }

var mailListTmpl = template.Must(template.New("maillist").Funcs(mailFuncs).Funcs(template.FuncMap{
	"btn": func(p *mailPage, id string) draftBtnArgs { return draftBtnArgs{p.CSRF, p.Box.ID, id} },
}).Parse(draftButtons + mailHead + `
{{if not .Box}}<section><p>None of the sprigs you can see has an email address yet. Ask Juni to set one up ("give Pip an email address").</p></section>
{{else}}{{$p := .}}
<section><h2>{{.Box.Name}} <span class="muted small">{{.Box.Address}}</span></h2>
<nav><a href="/dashboard/mail?sprig={{.Box.ID}}"{{if eq .Folder "inbox"}} aria-current="page"{{end}}>Inbox</a><a href="/dashboard/mail?sprig={{.Box.ID}}&amp;folder=drafts"{{if eq .Folder "drafts"}} aria-current="page"{{end}}>Drafts</a></nav>
<table><tr><th>Date</th><th>{{if eq .Folder "drafts"}}To{{else}}From{{end}}</th><th>Subject</th><th></th></tr>
{{range .Messages}}<tr><td class="small muted" style="white-space:nowrap">{{.Date}}</td><td>{{if $p.Folder | eq "drafts"}}{{join .To}}{{else}}{{.From}}{{end}}</td>
<td><a href="/dashboard/mail/message?sprig={{$p.Box.ID}}&amp;id={{.ID}}">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</a>{{if .Unread}} <span class="tag">new</span>{{end}}{{if .Attachments}} <span class="tag">📎 {{.Attachments}}</span>{{end}}<br><span class="muted small">{{.Preview}}</span></td>
<td>{{if .Draft}}{{template "draftbtns" (btn $p .ID)}}{{end}}</td></tr>
{{else}}<tr><td class="muted" colspan="4">Nothing here.</td></tr>{{end}}</table></section>
<section id="allowlist"><h2>Allowlist</h2><p class="muted small">Who {{.Box.Name}} may email without asking you (when its mail tools are at act level). Every recipient must be on it; anything else waits in "needs you". Replying to someone doesn't add them.</p>
<ul class="plain">{{range .Allow}}<li>{{.}}{{if $p.CanManage}} <form class="inline" method="post" action="/dashboard/mail/act"><input type="hidden" name="csrf" value="{{$p.CSRF}}"><input type="hidden" name="sprig" value="{{$p.Box.ID}}"><input type="hidden" name="entry" value="{{.}}"><button name="action" value="disallow">Remove</button></form>{{end}}</li>{{else}}<li class="muted">Empty: every email it writes is a draft for you to send.</li>{{end}}</ul>
{{if .CanManage}}<form method="post" action="/dashboard/mail/act" class="actions"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="sprig" value="{{.Box.ID}}"><input name="entry" maxlength="254" required placeholder="shop@vendor.example or @vendor.example" aria-label="Address or @domain"><button class="primary" name="action" value="allow">Add</button></form>
{{else}}<p class="muted small">Only its owner changes the allowlist.</p>{{end}}</section>
{{end}}</main></body></html>`))

var mailMsgTmpl = template.Must(template.New("mailmsg").Funcs(mailFuncs).Funcs(template.FuncMap{
	"btn": func(p *mailPage, id string) draftBtnArgs { return draftBtnArgs{p.CSRF, p.Box.ID, id} },
}).Parse(draftButtons + mailHead + `
{{with .Message}}{{$p := $}}<section><h2>{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</h2>
<table>
<tr><th>From</th><td>{{.From}}</td></tr>
<tr><th>To</th><td>{{join .To}}</td></tr>
{{if .Cc}}<tr><th>Cc</th><td>{{join .Cc}}</td></tr>{{end}}
{{if .Bcc}}<tr><th>Bcc</th><td>{{join .Bcc}}</td></tr>{{end}}
<tr><th>Date</th><td>{{.Date}}</td></tr>
<tr><th>Size</th><td>{{size .Size}}</td></tr>
</table>
<p class="muted small">Shown as plain text: no pictures are loaded, and links are written out with where they really go. Open one only if you trust it.</p>
<pre style="max-height:none;font-size:14px">{{.Text}}</pre>
{{if .Attachments}}<h3>Attachments</h3><ul class="plain">{{range .Attachments}}<li>{{.Name}} <span class="tag">{{.Type}}</span> <span class="muted small">{{size .Size}}</span> <a href="/dashboard/mail/attachment?sprig={{$p.Box.ID}}&amp;id={{$p.Message.ID}}&amp;aid={{.ID}}" download>Download</a></li>{{end}}</ul>
<p class="muted small">Attachments come from outside: download only what you expect, and open it with care.</p>{{end}}
{{if .Draft}}<div class="actions">{{template "draftbtns" (btn $p .ID)}}</div>{{end}}
<p><a href="/dashboard/mail?sprig={{$p.Box.ID}}{{if .Draft}}&amp;folder=drafts{{end}}">← Back</a></p></section>{{end}}
</main></body></html>`))

// mailDraft reports whether an inbox item's action is a mail draft
// (agent.MailDraftOf's shape: {"mail": {"sprig", "draft_id"}}).
func mailDraft(action string) bool {
	var a struct {
		Mail struct {
			Sprig   string `json:"sprig"`
			DraftID string `json:"draft_id"`
		} `json:"mail"`
	}
	return json.Unmarshal([]byte(action), &a) == nil && a.Mail.Sprig != "" && a.Mail.DraftID != ""
}

// CSRFToken is the form token for a session (for other packages' tests
// and tools that post the dashboard's forms).
func CSRFToken(session string) string { return csrfToken(session) }
