package gallery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/sprigpkg"
)

// BuildSite makes a gallery's static site from its repository: out gets
// index.html (every sprig with its drawn avatar, description, author and
// key fingerprint, the scopes it asks for and every version with its
// SHA-256), and copies of index.json and the packages, so out is the one
// folder to publish. THICKET_GALLERY_URL is then out's index.json.
func BuildSite(repo, out string) error {
	b, err := os.ReadFile(filepath.Join(repo, sprigpkg.IndexFile))
	if err != nil {
		return fmt.Errorf("no index.json (run thicket gallery build-index first): %w", err)
	}
	idx, err := sprigpkg.ParseIndex(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, p := range idx.Packages {
		for _, v := range p.Versions {
			raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(v.File)))
			if err != nil {
				return err
			}
			if s := sha256.Sum256(raw); hex.EncodeToString(s[:]) != v.SHA256 {
				return fmt.Errorf("%s doesn't match index.json: run thicket gallery verify", v.File)
			}
			dst := filepath.Join(out, filepath.FromSlash(v.File))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, raw, 0o644); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(out, sprigpkg.IndexFile), b, 0o644); err != nil {
		return err
	}
	var page bytes.Buffer
	if err := siteTmpl.Execute(&page, siteData{Index: idx, CSS: template.CSS(avatar.CSS)}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "index.html"), page.Bytes(), 0o644)
}

type siteData struct {
	Index *sprigpkg.Index
	CSS   template.CSS
}

var siteFuncs = template.FuncMap{
	// Avatar parameters are checked against fixed sets before they're
	// drawn, so the SVG holds nothing from the package but those.
	"avatar": func(p sprigpkg.IndexPackage) template.HTML {
		return template.HTML(avatar.Inline(p.Avatar, "idle", "", p.DisplayName))
	},
	"author": func(idx *sprigpkg.Index, key string) sprigpkg.IndexAuthor { return idx.Authors[key] },
	"revoked": func(idx *sprigpkg.Index, key string) bool {
		for _, r := range idx.Revoked {
			if r.KeyID == key {
				return true
			}
		}
		return false
	},
	"join": func(s []string) string { return strings.Join(s, ", ") },
}

var siteTmpl = template.Must(template.New("site").Funcs(siteFuncs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src 'self'">
<title>Thicket sprig gallery</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f7f5ef;color:#223}
main{max-width:860px;margin:0 auto;padding:16px}
article{background:#fff;border-radius:12px;padding:16px;margin:16px 0;display:flex;gap:16px;align-items:flex-start}
article svg{width:96px;height:96px;flex:none}
h1{margin:8px 0}h2{margin:0}
.muted{color:#667}.small{font-size:14px}
code{font-size:13px;word-break:break-all}
table{border-collapse:collapse;font-size:14px}td,th{padding:2px 8px 2px 0;text-align:left;vertical-align:top}
.revoked{color:#a33;font-weight:600}
@media (prefers-color-scheme:dark){body{background:#1b1d1a;color:#dde}article{background:#262924}.muted{color:#99a}}
{{.CSS}}
</style></head>
<body><main>
<h1>Thicket sprig gallery</h1>
<p>Sprigs shared by other Thicket households. To use this gallery, set <code>THICKET_GALLERY_URL</code> to this site's <a href="index.json">index.json</a>, then run <code>thicket gallery list</code> or ask Juni. Everything here was written by strangers: Thicket checks each package's signature and every file, and shows you all of it before anything is installed.</p>
<p class="muted small">{{len .Index.Packages}} sprigs{{if .Index.UpdatedAt}}, updated {{.Index.UpdatedAt}}{{end}}.</p>
{{$idx := .Index}}
{{range .Index.Packages}}{{$a := author $idx .Author}}
<article id="{{.Name}}">{{avatar .}}
<div>
<h2>{{.DisplayName}} <span class="muted small">{{.Name}}</span></h2>
<p>{{.Description}}</p>
<p class="small">By {{if $a.URL}}<a href="{{$a.URL}}" rel="nofollow noopener">{{$a.Name}}</a>{{else}}{{$a.Name}}{{end}}, author key <code>{{.Author}}</code>{{if revoked $idx .Author}} <span class="revoked">revoked</span>{{end}}<br>
Fingerprint <code>{{$a.Fingerprint}}</code></p>
<p class="small">Asks for: {{if .Scopes}}{{join .Scopes}}{{else}}no scopes{{end}} (none are granted when you install it).</p>
<table><tr><th>Version</th><th>Made</th><th>SHA-256</th></tr>
{{range .Versions}}<tr><td><a href="{{.File}}">{{.Version}}</a></td><td>{{.CreatedAt}}</td><td><code>{{.SHA256}}</code></td></tr>{{end}}
</table>
<p class="small">Install: <code>thicket gallery install {{.Name}}</code></p>
</div></article>
{{else}}<p>No sprigs yet.</p>{{end}}
{{if .Index.Revoked}}<h2>Revoked author keys</h2><ul>{{range .Index.Revoked}}<li><code>{{.KeyID}}</code> {{.RevokedAt}} {{.Reason}}</li>{{end}}</ul>{{end}}
</main></body></html>
`))
