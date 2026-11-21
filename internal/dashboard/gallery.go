package dashboard

import (
	"context"
	"crypto/hmac"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/gallery"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

// The Gallery page: the shared sprig gallery (docs/gallery.md), for
// adults when THICKET_GALLERY_URL is set. It lists the gallery's sprigs;
// "Preview" downloads one, checks it like any gallery import and shows
// everything it will get; "Install" installs exactly what was previewed.
// Names and descriptions come from strangers: the page escapes them, and
// the avatars are drawn from checked parameters only.

// GalleryEngine is what the Gallery page asks of the engine.
type GalleryEngine interface {
	GalleryOn() bool
	GalleryIndex(ctx context.Context, u *store.User) (*sprigpkg.Index, time.Time, error)
	GalleryRevoked(ctx context.Context) []string
	GalleryPreview(ctx context.Context, u *store.User, name string, version int) (preview, token string, err error)
	GalleryInstall(ctx context.Context, u *store.User, name string, version int, token string) (string, error)
}

func (d *Dashboard) galleryOn(u *store.User) bool {
	return d.Gallery != nil && d.Gallery.GalleryOn() && !u.IsKid()
}

type galleryItem struct {
	sprigpkg.IndexPackage
	Avatar          template.HTML
	Author          sprigpkg.IndexAuthor
	Revoked         bool
	LatestVersion   int
	ScopesAskedText string
}

type galleryPage struct {
	CSRF, Flash, Problem, Note string
	AvatarCSS                  template.CSS
	Items                      []galleryItem
	Fetched                    string
	// The preview.
	Name, Preview, Token string
	Version              int
}

// galleryUser checks the session and that the gallery is on for u.
func (d *Dashboard) galleryUser(w http.ResponseWriter, r *http.Request) (*store.User, *galleryPage, bool) {
	u, err := d.user(r)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, nil)
		return nil, nil, false
	}
	if !d.galleryOn(u) {
		http.Error(w, "no sprig gallery is set up here (THICKET_GALLERY_URL)", http.StatusNotFound)
		return nil, nil, false
	}
	c, _ := r.Cookie(SessionCookie)
	return u, &galleryPage{CSRF: csrfToken(c.Value), AvatarCSS: template.CSS(avatar.CSS)}, true
}

func (d *Dashboard) galleryPage(w http.ResponseWriter, r *http.Request) {
	u, p, ok := d.galleryUser(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("ok") == "installed" {
		p.Flash = "Installed. It has no scopes and every tool is at draft; its skills wait for your approval."
	}
	code := d.galleryList(r.Context(), u, p)
	d.galleryRender(w, code, galleryListTmpl, p)
}

// galleryList fills the list; it returns the status to answer with.
func (d *Dashboard) galleryList(ctx context.Context, u *store.User, p *galleryPage) int {
	idx, at, err := d.Gallery.GalleryIndex(ctx, u)
	if idx == nil {
		p.Problem = "Couldn't read the gallery: " + errText(err)
		return http.StatusBadGateway
	}
	if errors.Is(err, gallery.ErrStale) {
		p.Note = "Couldn't reach the gallery just now, so this is the list fetched earlier."
	}
	if !at.IsZero() {
		p.Fetched = at.Format("2 Jan 2006 15:04")
	}
	revoked := append(idx.RevokedIDs(), d.Gallery.GalleryRevoked(ctx)...)
	for _, pkg := range idx.Packages {
		it := galleryItem{IndexPackage: pkg, Author: idx.Authors[pkg.Author], Revoked: slices.Contains(revoked, pkg.Author), LatestVersion: pkg.Latest().Version,
			Avatar: template.HTML(avatar.Inline(pkg.Avatar, "idle", "", pkg.DisplayName))}
		it.ScopesAskedText = "no scopes"
		if len(pkg.Scopes) > 0 {
			it.ScopesAskedText = strings.Join(pkg.Scopes, ", ")
		}
		p.Items = append(p.Items, it)
	}
	return http.StatusOK
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func (d *Dashboard) galleryPreview(w http.ResponseWriter, r *http.Request) {
	u, p, ok := d.galleryUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p.Name = q.Get("name")
	p.Version, _ = strconv.Atoi(q.Get("version"))
	preview, token, err := d.Gallery.GalleryPreview(r.Context(), u, p.Name, p.Version)
	if err != nil {
		p.Problem = "Can't install " + p.Name + ": " + err.Error()
		d.galleryList(r.Context(), u, p)
		d.galleryRender(w, http.StatusBadRequest, galleryListTmpl, p)
		return
	}
	p.Preview, p.Token = preview, token
	d.galleryRender(w, http.StatusOK, galleryPreviewTmpl, p)
}

func (d *Dashboard) galleryInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	u, p, ok := d.galleryUser(w, r)
	if !ok {
		return
	}
	if !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(p.CSRF)) {
		http.Error(w, "this form has expired; reload the page", http.StatusForbidden)
		return
	}
	name := r.PostFormValue("name")
	version, _ := strconv.Atoi(r.PostFormValue("version"))
	if _, err := d.Gallery.GalleryInstall(r.Context(), u, name, version, r.PostFormValue("token")); err != nil {
		p.Problem = "Nothing was installed: " + err.Error()
		d.galleryList(r.Context(), u, p)
		d.galleryRender(w, http.StatusBadRequest, galleryListTmpl, p)
		return
	}
	http.Redirect(w, r, "/dashboard/gallery?"+url.Values{"ok": {"installed"}}.Encode(), http.StatusSeeOther)
}

func (d *Dashboard) galleryRender(w http.ResponseWriter, code int, t *template.Template, p *galleryPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := t.Execute(w, p); err != nil && d.Log != nil {
		d.Log.Error("gallery page", "err", err)
	}
}

const galleryHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Gallery · Thicket</title>` + style + `<style>{{.AvatarCSS}}
.gal{display:flex;gap:12px;align-items:flex-start}.gal svg{width:72px;height:72px;flex:none}</style></head>
<body><main>
<header><h1>🌱 Gallery</h1><a href="/dashboard">← Dashboard</a></header>
{{if .Flash}}<section class="flash" role="status">{{.Flash}}</section>{{end}}
{{if .Problem}}<section class="problem" role="alert">{{.Problem}}</section>{{end}}`

var galleryListTmpl = template.Must(template.New("gallerylist").Parse(galleryHead + `
<section><p class="muted small">Sprigs shared by other Thicket households. Everything here was written by strangers: Preview checks the package's signature and every file and shows you all of it. An installed sprig gets no scopes, every tool starts at draft, and its skills wait for your approval.{{if .Fetched}} List fetched {{.Fetched}}.{{end}}</p>
{{if .Note}}<p class="muted">{{.Note}}</p>{{end}}
{{range .Items}}<div class="gal" id="{{.Name}}">{{.Avatar}}<div>
<h2>{{.DisplayName}} <span class="muted small">{{.Name}}</span></h2>
<p>{{.Description}}</p>
<p class="small">By {{.Author.Name}}, author key <code>{{.IndexPackage.Author}}</code>{{if .Revoked}} <span class="tag">revoked</span>{{end}}<br><span class="muted">Fingerprint {{.Author.Fingerprint}}</span></p>
<p class="small">Asks for: {{.ScopesAskedText}} · version {{.LatestVersion}}</p>
{{if not .Revoked}}<form method="get" action="/dashboard/gallery/preview"><input type="hidden" name="name" value="{{.Name}}"><button class="primary">Preview</button></form>{{end}}
</div></div>{{else}}<p class="muted">The gallery is empty.</p>{{end}}</section>
</main></body></html>`))

var galleryPreviewTmpl = template.Must(template.New("gallerypreview").Parse(galleryHead + `
<section><h2>Preview: {{.Name}}</h2>
<p class="muted small">Read it all. Nothing is written until you press Install.</p>
<pre style="max-height:none;font-size:13px">{{.Preview}}</pre>
<form method="post" action="/dashboard/gallery/install" class="actions"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="name" value="{{.Name}}"><input type="hidden" name="version" value="{{.Version}}"><input type="hidden" name="token" value="{{.Token}}"><button class="primary">Install</button> <a href="/dashboard/gallery">Cancel</a></form>
</section></main></body></html>`))
