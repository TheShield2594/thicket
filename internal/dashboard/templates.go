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
button{font:inherit;background:none;border:1px solid var(--line);border-radius:8px;padding:4px 12px;color:var(--text);cursor:pointer;min-height:32px}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--bg)}
input,select{font:inherit;color:var(--text);background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:4px 8px;min-height:32px;max-width:100%}
label{display:inline-flex;align-items:center;gap:6px}
form.inline{display:inline}
.roster{display:grid;grid-template-columns:repeat(auto-fill,minmax(210px,1fr));gap:12px}
.sprigcard{display:flex;gap:10px;align-items:flex-start}.sprigcard svg{width:72px;height:72px;flex:none}
.state{font-size:12px;color:var(--muted)}.state b{color:var(--text);font-weight:600}
.flash{border-color:var(--accent)}.problem{border-color:var(--bad);color:var(--bad)}
.item{padding:10px 0;border-top:1px solid var(--line)}.item:first-of-type{border-top:0}
pre{white-space:pre-wrap;word-break:break-word;font-size:12px;background:var(--bg);border-radius:6px;padding:6px 8px;margin:6px 0;max-height:180px;overflow:auto}
.actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:6px}
</style>`

var signInTmpl = template.Must(template.New("signin").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Thicket</title>` + style + `</head>
<body><main><section><h1>Thicket</h1>
<p>The dashboard uses the web app's sign-in. <a href="/">Sign in there</a> with your username and password (or single sign-on, or a one-time link), then come back to <a href="/dashboard">/dashboard</a>.</p>
<p class="muted small">No password yet? Ask Juni for a web app link ("send me a web app link"), or run <code>thicket web link</code> on the server.</p>
</section></main></body></html>`))

// memTmpl is one memory on the memory screen: edit, lock, undo, delete.
const memTmpl = `{{define "mem"}}{{$p := .Page}}{{with .Mem}}<li>{{.Content}} <span class="tag">{{.Kind}}</span>{{if .Person}}<span class="tag">{{.Person}}</span>{{end}}{{if .Locked}}<span class="tag" title="Automatic review never changes it">kept as you said</span>{{end}}{{if .Due}}<span class="tag">follow up {{.Due}}</span>{{end}}
<br><span class="muted small">From {{.From}} · {{.When}}</span>
<div class="actions"><details><summary class="small">Edit</summary><form method="post" action="/dashboard/act" class="actions"><input type="hidden" name="csrf" value="{{$p.CSRF}}"><input type="hidden" name="kind" value="memory"><input type="hidden" name="id" value="{{.ID}}">
<input name="content" maxlength="300" required value="{{.Content}}" aria-label="Memory"><select name="mkind" aria-label="Kind">{{$k := .Kind}}{{range $p.MemKinds}}<option value="{{.}}"{{if eq . $k}} selected{{end}}>{{.}}</option>{{end}}</select><button class="primary" name="action" value="edit">Save</button></form></details>
<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$p.CSRF}}"><input type="hidden" name="kind" value="memory"><input type="hidden" name="id" value="{{.ID}}">{{if .Locked}}<button name="action" value="unlock" title="Let the automatic review update it">Unlock</button>{{else}}<button name="action" value="lock" title="Keep it as it is: the automatic review won't change it">Lock</button>{{end}}{{if .Undoable}} <button name="action" value="undo">Undo</button>{{end}} <button name="action" value="delete">Delete</button></form></div></li>{{end}}{{end}}`

type memArgsT struct {
	Page *pageData
	Mem  memView
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"memArgs": func(p *pageData, m memView) memArgsT { return memArgsT{p, m} },
}).Parse(memTmpl + `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><meta name="thicket-csrf" content="{{.CSRF}}"><title>Thicket</title>` + style + `<style>{{.AvatarCSS}}</style></head>
<body><main>
<header><h1>🌱 Thicket</h1>
<span class="muted small">{{.Name}}{{if .Admin}} (admin){{end}} · {{.Now}} · <a href="/">Open the chat app</a></span></header>
<nav><a href="#sprigs">Sprigs</a><a href="#inbox">Needs you{{with len .Inbox}} ({{.}}){{end}}</a><a href="#trail">Trail</a><a href="#memories">Memories</a><a href="#jobs">Jobs</a><a href="#skills">Skills</a><a href="#signin">Sign-in</a>{{if .Admin}}<a href="#costs">Costs</a><a href="#health">Health</a>{{end}}</nav>
{{if .Flash}}<section class="flash" role="status">{{.Flash}}</section>{{end}}
{{if .Problem}}<section class="problem" role="alert">{{.Problem}}</section>{{end}}
<p class="muted small">Mostly read-only: answer what needs you here, and ask a sprig for anything else. Thinking with {{.Model}}.</p>
{{if .PausedAll}}<section class="problem" role="status">⏸️ <b>Thicket is paused.</b> Nothing runs: no replies, jobs, triggers or check-ins.{{if .CanPauseAll}} <form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="kind" value="pause"><button class="primary" name="action" value="resume">Resume everything</button></form>{{else}} The owner can resume it.{{end}}</section>
{{else if .CanPauseAll}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="kind" value="pause"><button name="action" value="pause" title="Stop every reply, job and task now; nothing runs until you resume">⏸️ Pause everything</button></form>{{end}}

<section id="sprigs"><h2>Sprigs</h2><div class="roster">
{{range .Sprigs}}{{if .Private}}<div class="sprigcard"><div><b>{{.Name}}</b> <span class="tag">private</span>
<div class="state"><b>{{.State}}</b> · {{.Health}}</div>
<div class="muted small">Private to its owner. Thicket shows you only that it exists, how it's doing and (under Costs) what it costs: not its chats, memories, tools or activity. Everything is in the database file on the server, which Thicket won't open for you.</div></div></div>
{{else}}<div class="sprigcard">{{.Avatar}}<div><b>{{.Name}}</b>{{if .Host}} <span class="tag">host</span>{{end}}{{if .Only}} <span class="tag">{{.Only}}</span>{{end}}
<div class="state" id="st-{{.ID}}"><b>{{.State}}</b></div><div class="muted small">{{.Role}}</div>
{{if .Proactive}}<div class="muted small">{{.Proactive}}</div>{{end}}
<div>{{range .Scopes}}<span class="tag">{{.}}</span>{{else}}<span class="muted small">memory and teamwork only</span>{{end}}</div>
{{if .CanPause}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="pause"><input type="hidden" name="sprig" value="{{.ID}}">{{if .Paused}}<button class="primary" name="action" value="resume">Resume {{.Name}}</button>{{else}}<button name="action" value="pause">Pause {{.Name}}</button>{{end}}</form>{{end}}</div></div>{{end}}{{end}}
</div></section>

<section id="inbox"><h2>Needs you</h2>
{{range .Approvals}}<div class="item"><b>{{.Sprig}}</b> wants to: <b>{{.Tool}}</b> <span class="muted small">{{.At}}</span>
<pre>{{.Args}}</pre><div class="actions">
<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="approval"><input type="hidden" name="id" value="{{.ID}}"><button class="primary" name="action" value="approve">Approve</button></form>
<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="approval"><input type="hidden" name="id" value="{{.ID}}"><button name="action" value="deny">Deny</button></form>
</div></div>{{end}}
{{range .Inbox}}<div class="item"><span class="tag {{if eq .Kind "failed" "interrupted"}}bad{{end}}">{{.Kind}}</span><b>{{.Title}}</b>{{if gt .Count 1}} <span class="tag">×{{.Count}}</span>{{end}} <span class="muted small">{{.Sprig}} · {{.At}}</span>
{{if .Detail}}<pre>{{.Detail}}</pre>{{end}}<div class="actions">
{{if .Draft}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="inbox"><input type="hidden" name="id" value="{{.ID}}"><button class="primary" name="action" value="run">Do it</button></form>{{end}}
<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="inbox"><input type="hidden" name="id" value="{{.ID}}"><button name="action" value="resolve">{{if .Draft}}Dismiss{{else}}Clear{{end}}</button></form>
</div></div>{{end}}
{{if not (or .Approvals .Inbox)}}<p class="muted">Nothing needs you.</p>{{end}}
</section>

{{if not .User.IsKid}}<section id="changes"><h2>Changes</h2><p class="muted small">Permission changes and what sprigs put on the calendar{{if not .Admin}} for you{{end}}. Each can be undone.</p>
<table>{{range .Changes}}<tr><td class="small muted" style="white-space:nowrap">{{.At}}</td><td>{{if .Sprig}}<span class="tag">{{.Sprig}}</span>{{end}}<span class="tag{{if or (eq .Kind "safety") (eq .Kind "held")}} bad{{end}}">{{.Kind}}</span></td><td>{{.Summary}}{{if .Detail}} <span class="muted small">{{.Detail}}</span>{{end}}</td>
<td>{{if .Undoable}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="trail"><input type="hidden" name="id" value="{{.ID}}"><button name="action" value="undo">Undo</button></form>{{else if .Undone}}<span class="muted small">undone</span>{{end}}</td></tr>{{else}}<tr><td class="muted">No changes yet.</td></tr>{{end}}</table></section>{{end}}

<section id="trail"><h2>Trail</h2><p class="muted small">What sprigs did{{if not .Admin}} for you{{end}}, newest first.</p>
<table>{{range .Trail}}<tr><td class="small muted" style="white-space:nowrap">{{.At}}</td><td>{{if .Sprig}}<span class="tag">{{.Sprig}}</span>{{end}}<span class="tag">{{.Kind}}</span></td><td class="{{.Level}}">{{.Summary}}</td></tr>{{else}}<tr><td class="muted">Nothing yet.</td></tr>{{end}}</table></section>

<section id="memories"><h2>What they remember about you <span class="muted small">({{.MemCount}})</span></h2>
<p class="muted small">Facts are what sprigs know about you. Relationship memory is how things are going with each sprig: how you like it to talk to you, what landed, what annoyed you. Sprigs use your changes from their next reply. Edits are kept as you wrote them (locked) and can be undone; deleting forgets for good.</p>
{{range .MemBlocks}}{{if .Title}}<h3>{{.Title}}</h3><p class="muted small">{{.Note}}</p>{{end}}
<h3>Relationship</h3>{{range .Rel}}<h4 class="small">{{.Title}}</h4><ul class="plain">{{range .Memories}}{{template "mem" (memArgs $ .)}}{{end}}</ul>{{else}}<p class="muted small">Nothing yet.</p>{{end}}
<h3>Facts</h3>{{range .Facts}}<h4 class="small">{{.Title}}</h4><ul class="plain">{{range .Memories}}{{template "mem" (memArgs $ .)}}{{end}}</ul>{{else}}<p class="muted small">Nothing yet.</p>{{end}}
<details><summary class="small">Add a memory</summary>
<form method="post" action="/dashboard/act" class="actions"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="memory">
{{if .Sprig}}<input type="hidden" name="sprig" value="{{.Sprig}}">{{else}}<label class="small">With <select name="sprig">{{range .Sprigs}}<option value="{{.Value}}">{{.Label}}</option>{{end}}</select></label>{{end}}
{{if .Kids}}<label class="small">About <select name="person">{{range .Kids}}<option value="{{.Value}}">{{.Label}}</option>{{end}}</select></label>{{end}}
<label class="small">Kind <select name="mkind">{{range $.MemKinds}}<option value="{{.}}">{{.}}</option>{{end}}</select></label>
<input name="content" maxlength="300" required placeholder="Prefers short answers, no emoji." aria-label="Memory">
<button class="primary" name="action" value="add">Add</button></form>
<p class="muted small">tone, likes, dislikes, inside_joke and shared_history are relationship memory: pick the sprig it's with.</p></details>
{{end}}</section>

<section id="jobs"><h2>Jobs</h2>
{{range .Jobs}}<h3>#{{.ID}} {{.Name}} <span class="tag">{{.When}}</span><span class="tag">{{.Sprig}}</span>{{if not .Enabled}}<span class="tag warn">paused</span>{{end}}</h3>
{{if .Runs}}<table><tr><th>Ran</th><th>Result</th><th>Cost</th></tr>{{range .Runs}}<tr><td>{{.At}}</td><td class="{{.Status}}">{{.Status}}{{if .Error}} <span class="muted small">{{.Error}}</span>{{end}}</td><td>{{.Cost}}</td></tr>{{end}}</table>
{{else}}<p class="muted small">Not run yet.</p>{{end}}
{{else}}<p class="muted">No jobs. Ask a sprig for one: "every weekday at 7:30, summarise my day".</p>{{end}}
{{if .Tasks}}<h3>Background tasks</h3><p class="muted small">Longer work one sprig gave another. A task cut short by a restart is marked interrupted and isn't run again: ask for it again if you still want it.</p>
<table><tr><th>Started</th><th>Who</th><th>Task</th><th>Result</th></tr>{{range .Tasks}}<tr><td>{{.At}}</td><td>{{.Sprig}}{{if ne .Sprig .From}} <span class="muted small">from {{.From}}</span>{{end}}</td><td>{{.Task}}</td><td class="{{.Status}}">{{.Status}}{{if .Error}} <span class="muted small">{{.Error}}</span>{{end}}{{if .Ended}} <span class="muted small">{{.Ended}}</span>{{end}}</td></tr>{{end}}</table>{{end}}</section>

<section id="skills"><h2>Skills</h2><ul class="plain">
{{range .Skills}}<li><b>{{.Name}}</b> <span class="tag">{{.ID}} v{{.Version}}</span><br><span class="muted">{{.Description}}</span></li>{{else}}<li class="muted">None saved yet.</li>{{end}}
</ul></section>

<section id="signin"><h2>Sign-in</h2>
{{if .InviteLink}}<div class="item" role="status"><b>Invite link</b> (works once, for {{.InviteDays}} day{{if ne .InviteDays 1}}s{{end}}). Send it privately; it won't be shown again:<pre>{{.InviteLink}}</pre></div>{{end}}
<h3>Where you're signed in</h3><table><tr><th>Browser</th><th>How</th><th>Last active</th><th></th></tr>
{{range .Sessions}}<tr><td>{{.Device}}{{if .Current}} <span class="tag">this one</span>{{end}}</td><td>{{.Method}}</td><td>{{.Last}}</td><td>{{if not .Current}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="session"><input type="hidden" name="sid" value="{{.ID}}"><button name="action" value="revoke">Sign out</button></form>{{end}}</td></tr>{{end}}</table>
<p class="muted small">Set or change your password from the web app (the key button by your name).</p>
{{if .Admin}}<h3>Everyone's sessions</h3><table><tr><th>Who</th><th>Browser</th><th>How</th><th>Last active</th><th></th></tr>
{{range .AllSessions}}<tr><td>{{.Who}}</td><td>{{.Device}}{{if .Current}} <span class="tag">you, here</span>{{end}}</td><td>{{.Method}}</td><td>{{.Last}}</td><td>{{if not .Current}}<form class="inline" method="post" action="/dashboard/act"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="session"><input type="hidden" name="sid" value="{{.ID}}"><button name="action" value="revoke">Sign out</button></form>{{end}}</td></tr>{{end}}</table>
<h3>Invite someone</h3>
<form method="post" action="/dashboard/act" class="actions"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="kind" value="invite">
<label class="small">Name <input name="name" maxlength="60" placeholder="optional"></label>
<label class="small">As <select name="role"><option value="member">member</option><option value="admin">admin</option></select></label>
<label class="small">Valid for <select name="days"><option value="1">1 day</option><option value="3" selected>3 days</option><option value="7">7 days</option></select></label>
<button class="primary" name="action" value="create">Make invite link</button></form>
<p class="muted small">They choose a username and password (or use single sign-on, if it's set up). Members see the shared sprigs and their own.</p>{{end}}
</section>

{{if .Admin}}{{with .Spend}}<section id="costs"><h2>Costs</h2>
<div class="grid"><div class="stat"><span class="muted small">Today (UTC)</span><b>{{.Today}}</b><span class="muted small">{{.DailyCap}}</span></div>
<div class="stat"><span class="muted small">This month</span><b>{{.Month}}</b><span class="muted small">{{.MonthlyCap}}</span></div></div>
<h3>This month by sprig</h3><table><tr><th>Sprig</th><th>Heartbeat</th><th>Task</th><th>Total</th></tr>{{range $.Costs}}<tr><td>{{.Label}}</td><td>{{.Heartbeat}}</td><td>{{.Task}}</td><td><b>{{.Total}}</b></td></tr>{{else}}<tr><td class="muted" colspan="4">Nothing spent yet.</td></tr>{{end}}</table>
<p class="muted small">Heartbeat: jobs, triggers, check-ins and memory review. Task: chats and work you asked for.</p>
<h3>This month by model</h3><table><tr><th>Model</th><th>Calls</th><th>Cost</th></tr>{{range $.Models}}<tr><td>{{.Label}}</td><td>{{.Calls}}</td><td>{{.Total}}</td></tr>{{else}}<tr><td class="muted" colspan="3">None yet.</td></tr>{{end}}</table>
{{if .Mix}}<h3>What chat replies re-send</h3><table>
<tr><td>Earlier conversation</td><td class="barcell" style="width:55%">{{if .HistoryPct}}<div class="bar" style="width:{{.HistoryPct}}%"></div>{{end}}</td><td>{{.HistoryPct}}%</td></tr>
<tr><td>Kept PDF text</td><td class="barcell" style="width:55%">{{if .DocPct}}<div class="bar" style="width:{{.DocPct}}%"></div>{{end}}</td><td>{{.DocPct}}%</td></tr></table>
<p class="muted small">{{.Mix}} Now: history_tokens {{.HistorySet}}, document_chars {{.DocChars}}. Ask Juni to lower either to make each reply cheaper. Estimates: the provider reports one cost per call, split assuming output tokens cost 4× input.</p>{{end}}
<h3>Last 14 days</h3><table>{{range .ByDay}}<tr><td>{{.Label}}</td><td class="barcell" style="width:55%">{{if .Pct}}<div class="bar" style="width:{{.Pct}}%"></div>{{end}}</td><td>{{.Value}}</td></tr>{{end}}</table>
</section>{{end}}

<section id="health"><h2>Health</h2><table>{{range .Health}}<tr><td>{{.Name}}</td><td class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}ok{{else}}problem{{end}}</td><td class="muted small">{{.Detail}}</td></tr>{{end}}</table>
<p class="muted small">Thicket {{.Version}}</p></section>
{{end}}
</main>
<script nonce="{{.Nonce}}">
(function(){
  function tick(){
    fetch('/dashboard/state.json',{credentials:'same-origin'}).then(function(r){return r.ok?r.json():null}).then(function(st){
      if(!st)return;
      Object.keys(st).forEach(function(id){
        var s=st[id], av=document.getElementById('av-'+id), lb=document.getElementById('st-'+id);
        if(av){av.setAttribute('class','sprig is-'+s);var t=av.querySelector('title');if(t)t.textContent=t.textContent.replace(/ is \w+$/,' is '+s);}
        if(lb&&lb.firstChild)lb.firstChild.textContent=s;
      });
    }).catch(function(){});
  }
  setInterval(tick,5000);
})();
</script>
</body></html>`))
