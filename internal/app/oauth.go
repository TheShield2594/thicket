package app

import (
	"context"
	"html/template"
	"net/http"
	"time"
)

var oauthPage = template.Must(template.New("oauth").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Thicket</title>
<style>
:root{color-scheme:light dark;--bg:#f3f5fa;--fg:#151922;--muted:#5b6475}
@media (prefers-color-scheme:dark){:root{--bg:#0b0e15;--fg:#e8ebf2;--muted:#9aa3b5}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;padding:16px}
main{max-width:420px;text-align:center}h1{font-size:22px;margin:0 0 8px}p{color:var(--muted);margin:0}
</style></head><body><main><h1>{{.Title}}</h1><p>{{.Body}}</p></main></body></html>`))

// oauthCallback is where the browser lands after signing in to an
// integration. The state is single use and only Thicket knows it.
func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	name, err := a.OAuth.Callback(ctx, q.Get("state"), q.Get("code"), q.Get("error"), q.Get("error_description"))
	data := struct{ Title, Body string }{"Signed in", "You're signed in to " + name + ". You can close this tab: Juni will pick it up in your chat."}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		data = struct{ Title, Body string }{"Sign-in didn't work", err.Error()}
	}
	_ = oauthPage.Execute(w, data)
}
