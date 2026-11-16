package dashboard

import "html/template"

const style = `<style>
:root{--bg:#f7f6f2;--card:#fff;--text:#1f2a24;--muted:#5d6b63;--line:#e3e1d8;--accent:#3f7d4e;--warn:#9a6a00;--bad:#b3261e;--bar:#9cc7a6}
@media (prefers-color-scheme:dark){:root{--bg:#141815;--card:#1c211d;--text:#e6ebe7;--muted:#9aa79f;--line:#2c332e;--accent:#7cc28d;--warn:#e0b45a;--bad:#f2867e;--bar:#3f7d4e}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:960px;margin:0 auto;padding:16px}
header{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:8px}
h1{font-size:22px;margin:0}h2{font-size:17px;margin:0 0 8px}h3{font-size:14px;margin:12px 0 4px;color:var(--muted)}
nav{display:flex;gap:12px;flex-wrap:wrap;font-size:14px;margin:8px 0 16px}nav a{color:var(--accent)}
section{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;overflow-x:auto}
.muted{color:var(--muted)}.small{font-size:13px}
table{border-collapse:collapse;width:100%;font-size:14px}th,td{text-align:left;padding:6px 8px;border-top:1px solid var(--line);vertical-align:top}th{color:var(--muted);font-weight:600;border-top:0}
ul.plain{list-style:none;padding:0;margin:0}ul.plain li{padding:6px 0;border-top:1px solid var(--line)}ul.plain li:first-child{border-top:0}
.tag{display:inline-block;font-size:12px;padding:0 7px;border-radius:9px;border:1px solid var(--line);color:var(--muted);margin-right:4px;white-space:nowrap}
.ok{color:var(--accent)}.empty,.warn{color:var(--warn)}.error,.missed,.bad{color:var(--bad)}
.bar{background:var(--bar);height:10px;border-radius:5px}td.barcell{vertical-align:middle}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.stat b{display:block;font-size:20px}
button{font:inherit;background:none;border:1px solid var(--line);border-radius:8px;padding:4px 10px;color:var(--text);cursor:pointer}
</style>`

var signInTmpl = template.Must(template.New("signin").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Thicket</title>` + style + `</head>
<body><main><section><h1>Thicket</h1>
{{if .Problem}}<p>{{.Problem}}</p>{{end}}
<p>To see your dashboard, ask Juni for a sign-in link ("send me a dashboard link"). It arrives in your DMs and works once, for 10 minutes.</p>
<p class="muted small">On the server, <code>thicket dashboard-link</code> prints one too.</p>
</section></main></body></html>`))

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Thicket</title>` + style + `</head>
<body><main>
<header><h1>🌱 Thicket</h1>
<form method="post" action="/dashboard/logout"><span class="muted small">{{.Name}}{{if .Admin}} (admin){{end}} · {{.Now}} </span><button type="submit">Sign out</button></form></header>
<nav><a href="#sprigs">Sprigs</a><a href="#memories">Memories</a><a href="#jobs">Jobs</a><a href="#skills">Skills</a>{{if .Admin}}<a href="#spending">Spending</a><a href="#health">Health</a><a href="#activity">Activity</a>{{end}}</nav>
<p class="muted small">Read-only. To change anything, ask a sprig. Thinking with {{.Model}}.</p>

<section id="sprigs"><h2>Sprigs</h2><ul class="plain">
{{range .Sprigs}}<li><b>{{.Name}}</b>{{if .Host}} <span class="tag">host</span>{{end}} <span class="muted">{{.Role}}</span><br>
{{range .Scopes}}<span class="tag">{{.}}</span>{{else}}<span class="muted small">memory and teamwork only</span>{{end}}</li>{{end}}
</ul></section>

<section id="memories"><h2>What they remember about you <span class="muted small">({{.MemCount}})</span></h2>
{{range .MemGroup}}<h3>{{.Title}}</h3><ul class="plain">{{range .Memories}}
<li>{{.Content}} <span class="tag">{{.Kind}}</span>{{if .Locked}}<span class="tag" title="You set this; automatic review never changes it">kept as you said</span>{{end}}{{if .Due}}<span class="tag">follow up {{.Due}}</span>{{end}}</li>{{end}}
</ul>{{else}}<p class="muted">Nothing yet.</p>{{end}}
<p class="muted small">Say "forget that" to a sprig to remove something.</p></section>

<section id="jobs"><h2>Jobs</h2>
{{range .Jobs}}<h3>#{{.ID}} {{.Name}} <span class="tag">{{.When}}</span><span class="tag">{{.Sprig}}</span>{{if not .Enabled}}<span class="tag warn">paused</span>{{end}}</h3>
{{if .Runs}}<table><tr><th>Ran</th><th>Result</th><th>Cost</th></tr>{{range .Runs}}<tr><td>{{.At}}</td><td class="{{.Status}}">{{.Status}}{{if .Error}} <span class="muted small">{{.Error}}</span>{{end}}</td><td>{{.Cost}}</td></tr>{{end}}</table>
{{else}}<p class="muted small">Not run yet.</p>{{end}}
{{else}}<p class="muted">No jobs. Ask a sprig for one: "every weekday at 7:30, summarise my day".</p>{{end}}</section>

<section id="skills"><h2>Skills</h2><ul class="plain">
{{range .Skills}}<li><b>{{.Name}}</b> <span class="tag">{{.ID}} v{{.Version}}</span><br><span class="muted">{{.Description}}</span></li>{{else}}<li class="muted">None saved yet.</li>{{end}}
</ul></section>

{{if .Admin}}{{with .Spend}}<section id="spending"><h2>Spending</h2>
<div class="grid"><div class="stat"><span class="muted small">Today (UTC)</span><b>{{.Today}}</b><span class="muted small">{{.DailyCap}}</span></div>
<div class="stat"><span class="muted small">This month</span><b>{{.Month}}</b><span class="muted small">{{.MonthlyCap}}</span></div></div>
<h3>This month by sprig</h3><table>{{range .BySprig}}<tr><td>{{.Label}}</td><td class="barcell" style="width:45%">{{if .Pct}}<div class="bar" style="width:{{.Pct}}%"></div>{{end}}</td><td>{{.Value}}</td></tr>{{else}}<tr><td class="muted">Nothing spent yet.</td></tr>{{end}}</table>
<h3>Last 14 days</h3><table>{{range .ByDay}}<tr><td>{{.Label}}</td><td class="barcell" style="width:55%">{{if .Pct}}<div class="bar" style="width:{{.Pct}}%"></div>{{end}}</td><td>{{.Value}}</td></tr>{{end}}</table>
</section>{{end}}

<section id="health"><h2>Health</h2><table>{{range .Health}}<tr><td>{{.Name}}</td><td class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}ok{{else}}problem{{end}}</td><td class="muted small">{{.Detail}}</td></tr>{{end}}</table>
<p class="muted small">Thicket {{.Version}}</p></section>

<section id="activity"><h2>Recent activity</h2><table>{{range .Activity}}<tr><td class="small muted" style="white-space:nowrap">{{.At}}</td><td class="{{.Level}}">{{.Summary}}</td></tr>{{else}}<tr><td class="muted">Nothing yet.</td></tr>{{end}}</table></section>
{{end}}
</main></body></html>`))
