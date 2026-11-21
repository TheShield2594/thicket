package dashboard

import (
	"context"
	"crypto/hmac"
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/theshield2594/thicket/internal/store"
)

// The Browser page: which sites each sprig's own browser may load
// (docs/browser.md). Shown for the sprigs that have the browser and that
// u may see (a private sprig's only to its person; kids see none, kid
// sprigs have none); only a sprig's owner changes its list, and every
// change is in the trail (Undo there takes it back).

// BrowserEngine is what the Browser page asks of the engine.
type BrowserEngine interface {
	MaySeeBrowserOf(u *store.User, sprig string) bool
	CanManageBrowser(ctx context.Context, u *store.User, sprig string) bool
	BrowserAllowlist(ctx context.Context, u *store.User, sprig string) ([]string, error)
	ChangeBrowserAllow(ctx context.Context, by *store.User, sprig string, add, remove []string, via string) ([]string, error)
}

type browserSprig struct {
	ID, Name  string
	Allow     []string
	CanManage bool
}

type browserPage struct {
	CSRF, Flash, Problem string
	Sprigs               []browserSprig
}

// browserSprigs lists the sprigs with the browser that u may see.
func (d *Dashboard) browserSprigs(ctx context.Context, u *store.User) []browserSprig {
	if d.Browser == nil || u.IsKid() {
		return nil
	}
	var out []browserSprig
	for _, c := range d.Chars.All() {
		if c.IsKid() || !c.AllowsMCP("browser", "open") || !d.Browser.MaySeeBrowserOf(u, c.ID) {
			continue
		}
		out = append(out, browserSprig{ID: c.ID, Name: c.DisplayName(), CanManage: d.Browser.CanManageBrowser(ctx, u, c.ID)})
	}
	return out
}

func (d *Dashboard) browserUser(w http.ResponseWriter, r *http.Request) (*store.User, *browserPage, bool) {
	u, err := d.user(r)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, nil)
		return nil, nil, false
	}
	if d.Browser == nil {
		http.Error(w, "the browser plugin isn't running (docker compose --profile browser up -d)", http.StatusNotFound)
		return nil, nil, false
	}
	c, _ := r.Cookie(SessionCookie)
	return u, &browserPage{CSRF: csrfToken(c.Value), Sprigs: d.browserSprigs(r.Context(), u)}, true
}

func (d *Dashboard) browserList(r *http.Request, u *store.User, p *browserPage) {
	for i := range p.Sprigs {
		list, err := d.Browser.BrowserAllowlist(r.Context(), u, p.Sprigs[i].ID)
		if err != nil && p.Problem == "" {
			p.Problem = "Couldn't read an allowlist: " + err.Error()
		}
		p.Sprigs[i].Allow = list
	}
}

func (d *Dashboard) browserPageGet(w http.ResponseWriter, r *http.Request) {
	u, p, ok := d.browserUser(w, r)
	if !ok {
		return
	}
	switch r.URL.Query().Get("ok") {
	case "allowed":
		p.Flash = "Allowed. It's in the trail, and Undo there takes it back off."
	case "disallowed":
		p.Flash = "Taken off the allowlist: the browser can't load it from its next request."
	}
	d.browserList(r, u, p)
	d.browserRender(w, http.StatusOK, p)
}

func (d *Dashboard) browserAct(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	u, p, ok := d.browserUser(w, r)
	if !ok {
		return
	}
	if !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(p.CSRF)) {
		http.Error(w, "this form has expired; reload the page", http.StatusForbidden)
		return
	}
	sprig, entry := r.PostFormValue("sprig"), r.PostFormValue("entry")
	known := false
	for _, s := range p.Sprigs {
		known = known || s.ID == sprig
	}
	if !known { // not theirs to see, or no browser: the same answer
		http.Error(w, "no sprig with a browser you can see by that name", http.StatusNotFound)
		return
	}
	var err error
	done := ""
	switch r.PostFormValue("action") {
	case "allow":
		_, err = d.Browser.ChangeBrowserAllow(r.Context(), u, sprig, []string{entry}, nil, "from the dashboard")
		done = "allowed"
	case "disallow":
		_, err = d.Browser.ChangeBrowserAllow(r.Context(), u, sprig, nil, []string{entry}, "from the dashboard")
		done = "disallowed"
	default:
		err = errors.New("unknown action")
	}
	if err != nil {
		p.Problem = "Couldn't do that: " + err.Error()
		d.browserList(r, u, p)
		d.browserRender(w, http.StatusBadRequest, p)
		return
	}
	http.Redirect(w, r, "/dashboard/browser?"+url.Values{"ok": {done}}.Encode()+"#"+url.PathEscape(sprig), http.StatusSeeOther)
}

func (d *Dashboard) browserRender(w http.ResponseWriter, code int, p *browserPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := browserTmpl.Execute(w, p); err != nil {
		d.Log.Error("browser page", "err", err)
	}
}

var browserTmpl = template.Must(template.New("browser").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Browser · Thicket</title>` + style + `</head>
<body><main>
<header><h1>🌐 Browser</h1><a href="/dashboard">← Dashboard</a></header>
{{if .Flash}}<section class="flash" role="status">{{.Flash}}</section>{{end}}
{{if .Problem}}<section class="problem" role="alert">{{.Problem}}</section>{{end}}
<section><p class="muted small">The sites each sprig's own browser may load. A site's name allows its subdomains too. Everything else, page parts included, is blocked by the browser's proxy, and private or local addresses never load. Logins a sprig makes are kept, encrypted.</p></section>
{{range .Sprigs}}{{$s := .}}<section id="{{.ID}}"><h2>{{.Name}}</h2>
<ul class="plain">{{range .Allow}}<li>{{.}}{{if $s.CanManage}} <form class="inline" method="post" action="/dashboard/browser/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="sprig" value="{{$s.ID}}"><input type="hidden" name="entry" value="{{.}}"><button name="action" value="disallow">Remove</button></form>{{end}}</li>{{else}}<li class="muted">Empty: its browser can't load any site yet.</li>{{end}}</ul>
{{if .CanManage}}<form method="post" action="/dashboard/browser/act" class="actions"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="sprig" value="{{.ID}}"><input name="entry" maxlength="253" required placeholder="shop.example" aria-label="Site"><button class="primary" name="action" value="allow">Allow</button></form>
{{else}}<p class="muted small">Only its owner changes the allowlist.</p>{{end}}</section>
{{else}}<section><p>None of the sprigs you can see has the browser. Ask Juni to give one it ("give Pip the browser").</p></section>{{end}}
</main></body></html>`))
