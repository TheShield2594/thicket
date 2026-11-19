// Thicket web app. No framework, no build step: it ships inside the Go
// binary. State lives in S; render functions rebuild small regions.
'use strict';

const $app = document.getElementById('app');
const MOBILE = window.matchMedia('(max-width: 820px)');
const MAX_PANES = 3;
const TYPING_MS = 90_000;
const ROOM = 'all';

const S = {
  me: null,
  sprigs: new Map(),   // id -> sprig
  order: [],           // sprig ids, host first
  latest: new Map(),   // chat -> last message
  chats: new Map(),    // chat -> {messages, more, loaded, loading}
  typing: new Map(),   // chat -> timestamp
  routines: new Map(), // sprig id -> routines
  proactive: new Map(), // sprig id -> {daily_messages, sent_today, held}
  detailsAt: new Map(), // sprig id -> when its routines and proactive status were last fetched
  status: {},
  panes: [],           // open chat ids, left to right
  focus: 0,
  details: false,
  filter: '',
  drafts: new Map(),
  lastRead: load('thicket.read', {}),
  online: true,
  session: 0,          // bumped on sign-out; stale responses are dropped
  view: null,          // 'calendar', 'memory' or 'kid:<sprig id>' instead of chats
  memSprig: '',        // the memory screen's sprig filter ('' = all)
  cal: { start: null, layers: load('thicket.cal.layers', { events: true, jobs: true, outputs: true }), hide: load('thicket.cal.hide', {}) },
};

// ---------- small helpers ----------

function load(key, def) {
  try { const v = JSON.parse(localStorage.getItem(key)); return v ?? def; } catch { return def; }
}
function save(key, v) {
  try { localStorage.setItem(key, JSON.stringify(v)); } catch { /* private mode */ }
}

// h builds elements. Children may be strings (always text, never HTML),
// nodes, arrays, or falsy (skipped).
function h(tag, attrs, ...kids) {
  const [name, ...classes] = tag.split('.');
  const el = document.createElement(name || 'div');
  if (classes.length) el.className = classes.join(' ');
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className += (el.className ? ' ' : '') + v;
    else if (k === 'style' && typeof v === 'object') Object.entries(v).forEach(([p, val]) => el.style.setProperty(p, val));
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'html') el.innerHTML = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  const add = (c) => {
    if (c == null || c === false) return;
    if (Array.isArray(c)) c.forEach(add);
    else el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  };
  kids.forEach(add);
  return el;
}

const ICONS = {
  leaf: '<path d="M5 19c6 0 13-4 14-15-8 0-14 4-14 11v4z"/><path d="M5 19c3-4 6-6 10-8"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>',
  send: '<path d="M5 12h14"/><path d="m13 6 6 6-6 6"/>',
  split: '<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M12 4v16"/>',
  close: '<path d="M6 6l12 12M18 6 6 18"/>',
  info: '<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M15 4v16"/>',
  back: '<path d="m15 18-6-6 6-6"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon: '<path d="M20 14.5A8 8 0 1 1 9.5 4 6.5 6.5 0 0 0 20 14.5z"/>',
  auto: '<circle cx="12" cy="12" r="9"/><path d="M12 3v18" /><path d="M12 3a9 9 0 0 1 0 18z" fill="currentColor"/>',
  bell: '<path d="M6 8a6 6 0 1 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.9 1.9 0 0 0 3.4 0"/>',
  out: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
  home: '<path d="M3 11.5 12 4l9 7.5"/><path d="M5 10v10h14V10"/>',
  check: '<path d="m5 12 5 5 9-10"/>',
  x: '<path d="M7 7l10 10M17 7 7 17"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  sliders: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  key: '<circle cx="8" cy="15" r="4"/><path d="m10.8 12.2 8.7-8.7M17 6l2 2M15 8l2 2"/>',
  pin: '<path d="M9 4h6l-1 6 4 3v2H6v-2l4-3z"/><path d="M12 15v6"/>',
  brain: '<path d="M9.5 4a3 3 0 0 0-3 3 3 3 0 0 0-2 5 3 3 0 0 0 2 5 3 3 0 0 0 5.5 1.5V5.5A2.5 2.5 0 0 0 9.5 4z"/><path d="M14.5 4a3 3 0 0 1 3 3 3 3 0 0 1 2 5 3 3 0 0 1-2 5 3 3 0 0 1-5.5 1.5"/>',
  trash: '<path d="M4 7h16"/><path d="M10 11v6M14 11v6"/><path d="M6 7l1 13h10l1-13"/><path d="M9 7V4h6v3"/>',
  clip: '<path d="m20 11.5-8.2 8.2a5 5 0 0 1-7.1-7.1l8.6-8.6a3.4 3.4 0 0 1 4.8 4.8l-8.5 8.5a1.7 1.7 0 0 1-2.4-2.4l7.8-7.8"/>',
  mic: '<rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0"/><path d="M12 18v3"/>',
  stop: '<rect x="7" y="7" width="10" height="10" rx="2" fill="currentColor"/>',
  calendar: '<rect x="3" y="5" width="18" height="16" rx="3"/><path d="M3 10h18M8 3v4M16 3v4"/>',
  shield: '<path d="M12 3 5 6v5c0 4.5 3 8 7 10 4-2 7-5.5 7-10V6z"/>',
  link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
  memory: '<path d="M12 21s-7-4.4-7-10a4 4 0 0 1 7-2.6A4 4 0 0 1 19 11c0 5.6-7 10-7 10z"/>',
  users: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20a6.5 6.5 0 0 1 13 0"/><path d="M16 4.5a3.5 3.5 0 0 1 0 7M18 14a6.5 6.5 0 0 1 3.5 6"/>',
};
function icon(name, cls) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('class', 'i' + (cls ? ' ' + cls : ''));
  s.setAttribute('aria-hidden', 'true');
  s.innerHTML = ICONS[name];
  return s;
}

async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', credentials: 'same-origin', headers: { 'X-Thicket': '1' } };
  if (opts.body instanceof FormData) {
    init.body = opts.body; // the browser sets the multipart boundary
  } else if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, init);
  let data = {};
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
    // The session expired or was revoked: clean up as on sign-out.
    if (res.status === 401 && path !== '/api/login' && S.me) endSession();
    throw err;
  }
  return data;
}

const chatOf = (sprig) => `${S.me.user.id}.${sprig}`;
const sprigOf = (chat) => chat.slice(chat.indexOf('.') + 1);
const isRoom = (chat) => sprigOf(chat) === ROOM;

function initials(name) { return (name || '?').trim().charAt(0).toUpperCase(); }

function orb(sprig, size, opts = {}) {
  const el = h('span.orb', { style: { '--c': sprig?.color || '#7c8cff', '--s': size + 'px' }, 'aria-hidden': 'true' });
  if (sprig?.avatar) {
    const img = h('img', { src: sprig.avatar, alt: '', loading: 'lazy' });
    img.onerror = () => { img.remove(); el.textContent = initials(sprig.name); };
    el.append(img);
  } else {
    el.textContent = initials(sprig?.name);
  }
  if (opts.dot) el.append(h('span.dot'));
  return el;
}

function cluster() {
  return h('span.orb-cluster', { 'aria-hidden': 'true' }, S.order.slice(0, 3).map((id) => orb(S.sprigs.get(id), 24)));
}

function relTime(iso) {
  const d = new Date(iso);
  const s = (Date.now() - d) / 1000;
  if (s < 45) return 'now';
  if (s < 3600) return Math.round(s / 60) + 'm';
  if (s < 86400 && d.getDate() === new Date().getDate()) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (s < 6 * 86400) return d.toLocaleDateString([], { weekday: 'short' });
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}
function clock(iso) { return new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); }
function dayLabel(d) {
  const today = new Date(); const y = new Date(Date.now() - 86400000);
  if (d.toDateString() === today.toDateString()) return 'Today';
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  return d.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' });
}
function plain(md) {
  return md.replace(/```[\s\S]*?```/g, '[code]').replace(/[*_`#>]/g, '').replace(/\s+/g, ' ').trim();
}

let toastTimer;
function toast(text) {
  document.querySelector('.toast')?.remove();
  const t = h('div.toast', { role: 'status' }, text);
  document.body.append(t);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 3500);
}

// ---------- markdown (a safe subset) ----------

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const esc = (s) => s.replace(/[&<>"']/g, (c) => ESC[c]);

function inline(src) {
  const codes = [];
  let s = esc(src).replace(/`([^`\n]+)`/g, (_, c) => { codes.push(c); return `\u0000${codes.length - 1}\u0000`; });
  s = s
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
    .replace(/(^|[\s(])(https?:\/\/[^\s<)]+[^\s<).,!?:;'"])/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>')
    .replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[^*\w])\*([^*\n]+)\*(?!\w)/g, '$1<em>$2</em>')
    .replace(/(^|[^_\w])_([^_\n]+)_(?!\w)/g, '$1<em>$2</em>')
    .replace(/~~([^~\n]+)~~/g, '<del>$1</del>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[+i]}</code>`);
}

function blocks(src) {
  const out = [];
  let para = [], list = null;
  const flushPara = () => { if (para.length) out.push('<p>' + para.map(inline).join('<br>') + '</p>'); para = []; };
  const flushList = () => { if (list) out.push(`<${list.tag}>` + list.items.map((i) => `<li>${inline(i)}</li>`).join('') + `</${list.tag}>`); list = null; };
  for (const line of src.split('\n')) {
    let m;
    if (!line.trim()) { flushPara(); flushList(); continue; }
    if ((m = line.match(/^\s*[-*•]\s+(.*)$/))) { flushPara(); if (list?.tag !== 'ul') { flushList(); list = { tag: 'ul', items: [] }; } list.items.push(m[1]); continue; }
    if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) { flushPara(); if (list?.tag !== 'ol') { flushList(); list = { tag: 'ol', items: [] }; } list.items.push(m[1]); continue; }
    flushList();
    if ((m = line.match(/^#{1,4}\s+(.*)$/))) { flushPara(); out.push(`<h4>${inline(m[1])}</h4>`); continue; }
    if ((m = line.match(/^>\s?(.*)$/))) { flushPara(); out.push(`<blockquote>${inline(m[1])}</blockquote>`); continue; }
    para.push(line);
  }
  flushPara(); flushList();
  return out.join('');
}

function md(src) {
  return src.split('```').map((part, i) => {
    if (i % 2 === 0) return blocks(part);
    const nl = part.indexOf('\n');
    const body = nl >= 0 && /^[\w+#.-]*$/.test(part.slice(0, nl).trim()) ? part.slice(nl + 1) : part;
    return `<pre><code>${esc(body.replace(/\n$/, ''))}</code></pre>`;
  }).join('');
}

// ---------- boot & sign-in ----------

async function boot() {
  if (location.pathname === '/login') return showLogin();
  if (location.pathname === '/invite') return showInvite();
  try {
    await refreshMe();
  } catch (e) {
    if (e.status === 401) return showSignedOut();
    return showGate('Thicket isn’t answering', 'Couldn’t reach the server (' + e.message + '). It may be restarting.', h('button.btn', { onclick: () => location.reload() }, 'Try again'));
  }
  restoreLayout();
  renderShell();
  connect();
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
}

function showGate(title, text, ...rest) {
  $app.className = '';
  $app.replaceChildren(h('div.gate', {}, h('div.gate-card', {},
    h('div.hero-orb', { 'aria-hidden': 'true' }), h('h1', {}, title), text && h('p', {}, text), ...rest)));
}

function showLogin() {
  const token = location.hash.slice(1);
  const problem = new URLSearchParams(location.search).get('error');
  history.replaceState(null, '', '/login'); // keep the token out of history
  if (!token) return showSignedOut(problem);
  const err = h('p.err', { role: 'alert' });
  const btn = h('button.btn', {
    onclick: async () => {
      btn.disabled = true; btn.textContent = 'Signing in…'; err.textContent = '';
      try {
        await api('/api/login', { method: 'POST', body: { token } });
        location.replace('/');
      } catch (e) {
        err.textContent = e.message; btn.disabled = false; btn.textContent = 'Continue';
      }
    },
  }, 'Continue');
  // A button press (not the page load) redeems the link, so link
  // previews in chat apps can't use it up.
  showGate('Welcome to Thicket', 'Your sprigs are ready when you are.', btn, err);
  btn.focus();
}

function field(label, attrs) {
  const input = h('input.field', attrs);
  return [h('label.field-label', {}, h('span', {}, label), input), input];
}

// oidcButton starts single sign-on (with an invite, if there is one).
function oidcButton(name, invite) {
  const err = h('p.err', { role: 'alert' });
  const btn = h('button.btn.secondary', {
    type: 'button',
    onclick: async () => {
      btn.disabled = true; err.textContent = '';
      try {
        const r = await api('/api/oidc/start', { method: 'POST', body: { invite: invite || '' } });
        location.assign(r.url);
      } catch (e) { err.textContent = e.message; btn.disabled = false; }
    },
  }, 'Continue with ' + name);
  return [btn, err];
}

async function showSignedOut(problem) {
  let opts = { password: true };
  try { opts = await api('/api/auth'); } catch { /* offer the basics */ }
  const [userLabel, user] = field('Username', { name: 'username', autocomplete: 'username', autocapitalize: 'none', spellcheck: 'false', required: true });
  const [pwLabel, pw] = field('Password', { name: 'password', type: 'password', autocomplete: 'current-password', required: true });
  const err = h('p.err', { role: 'alert' }, problem || '');
  const btn = h('button.btn', { type: 'submit' }, 'Sign in');
  const form = h('form.signin', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      btn.disabled = true; btn.textContent = 'Signing in…'; err.textContent = '';
      try {
        await api('/api/login', { method: 'POST', body: { username: user.value.trim(), password: pw.value } });
        location.replace('/');
      } catch (e) {
        err.textContent = e.message; btn.disabled = false; btn.textContent = 'Sign in'; pw.select();
      }
    },
  }, userLabel, pwLabel, btn);
  showGate('Sign in to Thicket', null,
    opts.oidc && oidcButton(opts.oidc.name),
    opts.oidc && h('div.or', {}, 'or'),
    form, err,
    h('details.link-help', {}, h('summary', {}, 'No password? Use a one-time link'),
      h('div.steps', {},
        h('div', {}, h('b', {}, '1'), h('span', {}, 'In a Discord or Telegram DM, ask Juni for a “web app link”.')),
        h('div', {}, h('b', {}, '2'), h('span', {}, 'Or on the server run ', h('code', {}, 'thicket web link'), '.')))));
  (opts.oidc ? null : user)?.focus();
}

async function showInvite() {
  const token = location.hash.slice(1);
  history.replaceState(null, '', '/invite'); // keep the token out of history
  if (!token) return showSignedOut('That invite link is incomplete. Copy the whole link and try again.');
  let inv;
  try {
    inv = await api('/api/invite/check', { method: 'POST', body: { token } });
  } catch (e) { return showSignedOut(e.message); }
  const [userLabel, user] = field('Choose a username', { name: 'username', autocomplete: 'username', autocapitalize: 'none', spellcheck: 'false', required: true, minlength: 2, maxlength: 32 });
  const [pwLabel, pw] = field('Choose a password', { name: 'password', type: 'password', autocomplete: 'new-password', required: true, minlength: 10 });
  const [againLabel, again] = field('Type it again', { name: 'again', type: 'password', autocomplete: 'new-password', required: true });
  const err = h('p.err', { role: 'alert' });
  const btn = h('button.btn', { type: 'submit' }, 'Create my account');
  const form = h('form.signin', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      err.textContent = '';
      if (pw.value !== again.value) { err.textContent = 'The two passwords don’t match.'; return; }
      btn.disabled = true; btn.textContent = 'Setting up…';
      try {
        await api('/api/invite', { method: 'POST', body: { token, username: user.value.trim(), password: pw.value } });
        location.replace('/');
      } catch (e) { err.textContent = e.message; btn.disabled = false; btn.textContent = 'Create my account'; }
    },
  }, userLabel, pwLabel, h('small.hint', {}, 'At least 10 characters. A few words make a good one.'), againLabel, btn);
  showGate(inv.name ? `Welcome, ${inv.name}` : 'You’re invited', 'You’ve been invited to this Thicket' + (inv.role === 'admin' ? ' as an admin' : '') + '.',
    inv.oidc && oidcButton(inv.oidc.name, token), inv.oidc && h('div.or', {}, 'or'), form, err);
  user.focus();
}

// ---------- account ----------

async function showAccount() {
  let acct;
  try { acct = await api('/api/account'); } catch (e) { return toast('Couldn’t load your account: ' + e.message); }
  const close = () => sheet.remove();
  const err = h('p.err', { role: 'alert' });
  const fields = [];
  let user;
  if (!acct.username) {
    const [l, i] = field('Username', { autocomplete: 'username', autocapitalize: 'none', spellcheck: 'false', required: true });
    fields.push(l); user = i;
  }
  let cur;
  if (acct.has_password) {
    const [l, i] = field('Current password', { type: 'password', autocomplete: 'current-password', required: true });
    fields.push(l); cur = i;
  }
  const [nl, next] = field(acct.has_password ? 'New password' : 'Password', { type: 'password', autocomplete: 'new-password', required: true, minlength: 10 });
  fields.push(nl);
  const btn = h('button.btn', { type: 'submit' }, acct.has_password ? 'Change password' : 'Set password');
  const form = h('form.signin', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      btn.disabled = true; err.textContent = '';
      try {
        const r = await api('/api/account/password', { method: 'POST', body: { username: user?.value.trim() || '', current: cur?.value || '', new: next.value } });
        close();
        toast(r.signed_out ? `Password saved. Signed out ${r.signed_out} other session${r.signed_out === 1 ? '' : 's'}.` : 'Password saved.');
      } catch (e) { err.textContent = e.message; btn.disabled = false; }
    },
  }, fields, btn);
  const me = S.me.user;
  const [nameL, nameI] = field('Name (what sprigs call you)', { autocomplete: 'name', maxlength: 60, value: me.name || '' });
  const [tzL, tzI] = field('Timezone', { placeholder: 'America/New_York', autocapitalize: 'none', spellcheck: 'false', value: me.timezone || '', list: 'tz-list' });
  let zones = [];
  try { zones = Intl.supportedValuesOf('timeZone'); } catch { /* an older browser: type it */ }
  const here = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const pErr = h('p.err', { role: 'alert' });
  const pBtn = h('button.btn', { type: 'submit' }, 'Save');
  const profile = h('form.signin', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      pBtn.disabled = true; pErr.textContent = '';
      try {
        await api('/api/account/profile', { method: 'POST', body: { name: nameI.value.trim(), timezone: tzI.value.trim() } });
        await refreshMe(); renderSidebar();
        toast('Saved.');
      } catch (e) { pErr.textContent = e.message; }
      pBtn.disabled = false;
    },
  }, nameL, tzL, h('datalist', { id: 'tz-list' }, zones.map((z) => h('option', { value: z }))),
  !me.timezone && here && h('small.hint', {}, 'This device is on ', h('button.linkish', { type: 'button', onclick: () => { tzI.value = here; } }, here), '.'),
  pErr, pBtn);
  const rows = acct.sessions.map((s) => h('li.session', {},
    h('div', {}, h('b', {}, s.device), s.current && h('span.tag', {}, 'this one'),
      h('small', {}, `${{ link: 'sign-in link', password: 'password', oidc: 'single sign-on', invite: 'invite' }[s.method] || s.method} · active ${relTime(s.last_seen)}`)),
    !s.current && h('button.chip', {
      onclick: async (ev) => {
        try { await api('/api/account/sessions/' + encodeURIComponent(s.id), { method: 'DELETE' }); ev.target.closest('li').remove(); toast('Signed out'); }
        catch (e) { toast('Couldn’t sign it out: ' + e.message); }
      },
    }, 'Sign out')));
  const sheet = h('div.sheet', { role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Your account', onclick: (ev) => { if (ev.target === sheet) close(); } },
    h('div.sheet-card', {},
      h('div.sheet-head', {}, h('h2', {}, 'Your account'), h('button.icon-btn', { 'aria-label': 'Close', onclick: close }, icon('x'))),
      profile,
      h('h3', {}, 'Signing in'),
      acct.username ? h('p.muted', {}, 'Username: ', h('b', {}, acct.username)) : h('p.muted', {}, 'Set a username and password to sign in without a link.'),
      acct.oidc && h('p.muted', {}, 'You can also sign in with single sign-on.'),
      form, err,
      h('h3', {}, 'Where you’re signed in'),
      h('ul.sessions', {}, rows)));
  sheet.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') close(); });
  document.body.append(sheet);
  (me.name ? user || cur || next : nameI).focus();
}

function relTime(at) {
  const s = (Date.now() - new Date(at)) / 1000;
  if (s < 90) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

async function refreshMe() {
  const session = S.session;
  const me = await api('/api/me');
  if (session !== S.session) throw new Error('signed out');
  S.me = me;
  S.status = me.status || {};
  S.sprigs = new Map(me.sprigs.map((s) => [s.id, s]));
  S.order = me.sprigs.map((s) => s.id);
  S.latest = new Map(me.latest.map((m) => [m.chat, m]));
  // Drop panes whose sprig has gone.
  S.panes = S.panes.filter((c) => isRoom(c) || S.sprigs.has(sprigOf(c)));
}

function restoreLayout() {
  const saved = load('thicket.layout', null);
  const path = location.pathname.match(/^\/c\/([\w-]+)$/);
  if (path && (path[1] === ROOM || S.sprigs.has(path[1]))) {
    S.panes = [chatOf(path[1])];
  } else if (saved?.user === S.me.user.id) {
    S.panes = (saved.panes || []).filter((c) => c.startsWith(S.me.user.id + '.') && (isRoom(c) || S.sprigs.has(sprigOf(c))));
    S.details = !!saved.details && !MOBILE.matches;
  }
  if (MOBILE.matches) S.panes = S.panes.slice(0, 1);
  S.focus = Math.min(S.focus, Math.max(0, S.panes.length - 1));
}

function persistLayout() {
  save('thicket.layout', { user: S.me.user.id, panes: S.panes, details: S.details });
  const chat = S.panes[S.focus];
  const url = chat ? '/c/' + sprigOf(chat) : '/';
  if (location.pathname !== url) history.replaceState(null, '', url);
}

// ---------- live events ----------

let es, wasDown = false, downTimer;
function connect() {
  es?.close();
  es = new EventSource('/api/events');
  es.onopen = () => {
    clearTimeout(downTimer);
    if (wasDown) { wasDown = false; S.online = true; resync(); }
  };
  es.onerror = () => {
    wasDown = true;
    clearTimeout(downTimer);
    downTimer = setTimeout(async () => {
      S.online = false; renderSidebar();
      try { await api('/api/me'); } catch (e) { if (e.status === 401) endSession(); }
    }, 4000);
  };
  es.onmessage = (e) => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    if (ev.type === 'message') onMessage(ev.chat, ev.message);
    else if (ev.type === 'typing') onTyping(ev.chat);
    else if (ev.type === 'deleted') onDeleted(ev.chat, ev.id);
    else if (ev.type === 'sprigs') onRoster();
    else if (ev.type === 'audio') onAudio(ev.chat, ev.id);
  };
}

// A spoken reply arrived for a message already shown: add its player.
function onAudio(chat, id) {
  const m = S.chats.get(chat)?.messages.find((x) => x.id === id);
  if (!m) return;
  m.audio = true;
  renderFeed(chat);
}

// A sprig was created, changed or removed: reload the roster so it shows
// up in the sidebar straight away. Several events in a row load once.
let rosterTimer;
function onRoster() {
  clearTimeout(rosterTimer);
  rosterTimer = setTimeout(async () => {
    const had = new Set(S.order);
    try { await refreshMe(); } catch { return; }
    // Settings holds forms someone may be half-way through (a token, an
    // unsaved value); only the sidebar needs the new roster there.
    if (S.view === 'settings') renderSidebar(); else renderShell();
    for (const id of S.order) if (!had.has(id)) toast(`${S.sprigs.get(id).name} has joined. Find them in the sidebar.`);
  }, 300);
}

async function resync() {
  try { await refreshMe(); } catch { return; }
  for (const chat of S.panes) await loadChat(chat, { fresh: true });
  if (S.me) renderShell();
}

function onMessage(chat, m) {
  const c = S.chats.get(chat);
  // Keep it even while the chat's first load (or a retry) is in flight:
  // loadChat merges what arrived here with its snapshot by id.
  if (c && !c.messages.some((x) => x.id === m.id)) {
    // Replace the optimistic copy of our own message if it's still there.
    const i = m.role === 'user' ? c.messages.findIndex((x) => x.pending && x.content === m.content) : -1;
    if (i >= 0) c.messages.splice(i, 1, m); else c.messages.push(m);
  }
  if (m.role === 'assistant') {
    S.typing.delete(chat);
    notify(chat, m);
    // A sprig's message can change today's count or release a held one:
    // mark its details stale, and refresh them if they're showing.
    S.detailsAt.delete(sprigOf(chat));
    if (S.details && sprigOf(S.panes[S.focus] || '') === sprigOf(chat)) renderDetails();
  }
  S.latest.set(chat, m);
  if (S.panes.includes(chat) && document.visibilityState === 'visible') markRead(chat);
  renderFeed(chat, m.id);
  renderSidebar();
  updatePaneHead(chat);
}

function onTyping(chat) {
  S.typing.set(chat, Date.now());
  renderFeed(chat);
  renderSidebar();
  updatePaneHead(chat);
  setTimeout(() => { if (Date.now() - (S.typing.get(chat) || 0) >= TYPING_MS) { S.typing.delete(chat); renderFeed(chat); renderSidebar(); updatePaneHead(chat); } }, TYPING_MS + 50);
}

function onDeleted(chat, id) {
  const c = S.chats.get(chat);
  if (c) c.messages = c.messages.filter((m) => m.id !== id);
  if (S.latest.get(chat)?.id === id) S.latest.delete(chat);
  renderFeed(chat);
  renderSidebar();
}

const isTyping = (chat) => Date.now() - (S.typing.get(chat) || 0) < TYPING_MS;

function markRead(chat) {
  const last = S.latest.get(chat);
  if (last && (S.lastRead[chat] || 0) < last.id) { S.lastRead[chat] = last.id; save('thicket.read', S.lastRead); }
}
const unread = (chat) => { const l = S.latest.get(chat); return !!l && l.role === 'assistant' && l.id > (S.lastRead[chat] || 0); };

function notify(chat, m) {
  if (document.visibilityState === 'visible' && S.panes.includes(chat)) return;
  if (!('Notification' in window) || Notification.permission !== 'granted' || !load('thicket.notify', false)) return;
  const s = S.sprigs.get(m.character);
  try {
    const n = new Notification(s?.name || 'Thicket', { body: plain(m.content).slice(0, 180), tag: chat, icon: '/app/icon-192.png' });
    // In the desktop app, thicketDesktop brings a hidden window back.
    n.onclick = () => { window.thicketDesktop?.show(); window.focus(); openChat(chat); n.close(); };
  } catch { /* some platforms only allow notifications from the service worker */ }
}

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && S.me) { S.panes.forEach(markRead); renderSidebar(); }
});

// ---------- data ----------

async function loadChat(chat, opts = {}) {
  let c = S.chats.get(chat);
  if (!c) { c = { messages: [], more: false, loaded: false, loading: false, error: '' }; S.chats.set(chat, c); }
  if (c.loading || (c.loaded && !opts.fresh && !opts.older)) return;
  c.loading = true;
  const session = S.session;
  try {
    const before = opts.older && c.messages.length ? c.messages[0].id : 0;
    const res = await api(`/api/chats/${encodeURIComponent(chat)}/messages${before ? '?before=' + before : ''}`);
    if (session !== S.session) return; // signed out meanwhile
    if (opts.older) {
      c.messages = res.messages.concat(c.messages);
    } else {
      // Keep what arrived over the event stream while this request was in
      // flight (newer than the snapshot), plus unsent messages.
      const ids = new Set(res.messages.map((m) => m.id));
      const top = res.messages.length ? res.messages[res.messages.length - 1].id : 0;
      const live = c.messages.filter((m) => m.pending || m.failed || (m.id > top && !ids.has(m.id)));
      c.messages = res.messages.concat(live);
    }
    c.more = res.more;
    c.loaded = true;
    c.error = '';
  } catch (e) {
    if (session !== S.session) return;
    if (!c.loaded) c.error = e.message || 'network error';
    else toast('Couldn’t refresh messages: ' + e.message);
  } finally {
    c.loading = false;
  }
}

async function loadRoutines(id) {
  const session = S.session;
  S.detailsAt.set(id, Date.now()); // also stops renderDetails asking again meanwhile
  let routines = null, proactive = null, failed = false;
  try { ({ routines, proactive } = await api('/api/sprigs/' + encodeURIComponent(id))); } catch { failed = true; }
  if (session !== S.session) return; // signed out meanwhile
  if (failed && S.routines.get(id)) return; // a refresh failed: keep what's shown
  S.routines.set(id, routines);
  S.proactive.set(id, proactive || null);
  renderDetails();
}

let tempId = -1;
// fileLine is how a sent file shows in the chat (the server stores the same).
const fileLine = (f) => (f.type.startsWith('audio/') ? '🎤 ' : '📎 ') + f.name;

async function send(chat, text, files = []) {
  text = text.trim();
  if (!text && !files.length) return;
  const c = S.chats.get(chat);
  const content = [text, ...files.map(fileLine)].filter(Boolean).join('\n');
  const m = { id: tempId--, chat, role: 'user', content, text, files, at: new Date().toISOString(), pending: true };
  c.messages.push(m);
  renderFeed(chat, m.id, true);
  const session = S.session;
  try {
    const res = await api(`/api/chats/${encodeURIComponent(chat)}/messages`, { method: 'POST', body: files.length ? formOf(text, files) : { text } });
    if (session !== S.session) return; // signed out meanwhile
    const i = c.messages.indexOf(m);
    const dup = c.messages.some((x) => x.id === res.message.id);
    if (i >= 0) { if (dup) c.messages.splice(i, 1); else c.messages.splice(i, 1, res.message); }
    if ((S.latest.get(chat)?.id || 0) < res.message.id) S.latest.set(chat, res.message);
    renderSidebar();
  } catch (e) {
    if (session !== S.session) return;
    m.pending = false; m.failed = e.message || 'Not sent';
  }
  renderFeed(chat);
}

function formOf(text, files) {
  const fd = new FormData();
  fd.append('text', text);
  for (const f of files) fd.append('file', f, f.name);
  return fd;
}

// ---------- layout actions ----------

async function openChat(chat, opts = {}) {
  S.view = null;
  const at = S.panes.indexOf(chat);
  if (at >= 0) {
    S.focus = at;
  } else if (opts.split && !MOBILE.matches && S.panes.length > 0) {
    if (S.panes.length >= MAX_PANES) S.panes.splice(S.focus === 0 ? 1 : 0, 1);
    S.panes.push(chat);
    S.focus = S.panes.length - 1;
  } else if (S.panes.length) {
    S.panes[S.focus] = chat;
  } else {
    S.panes = [chat];
    S.focus = 0;
  }
  persistLayout();
  renderShell();
  await loadChat(chat);
  if (!S.me) return; // signed out meanwhile
  markRead(chat);
  renderFeed(chat);
  renderSidebar();
  if (!MOBILE.matches) paneEls.get(chat)?.querySelector('textarea')?.focus();
}

function closePane(chat) {
  const i = S.panes.indexOf(chat);
  if (i < 0) return;
  S.panes.splice(i, 1);
  S.focus = Math.max(0, Math.min(S.focus, S.panes.length - 1));
  paneEls.delete(chat);
  persistLayout();
  renderShell();
}

function goHome() {
  S.view = null;
  S.panes = [];
  S.focus = 0;
  persistLayout();
  renderShell();
}

function toggleDetails(force) {
  S.details = force ?? !S.details;
  persistLayout();
  renderMain();
}

function cycleTheme() {
  const cur = document.documentElement.dataset.theme || 'auto';
  const next = { auto: 'dark', dark: 'light', light: 'auto' }[cur];
  if (next === 'auto') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = next;
  try { if (next === 'auto') localStorage.removeItem('thicket.theme'); else localStorage.setItem('thicket.theme', next); } catch { /* ignore */ }
  renderSidebar();
  toast({ auto: 'Theme follows your system', dark: 'Dark theme', light: 'Light theme' }[next]);
}

async function toggleNotify() {
  if (!('Notification' in window)) return toast('This browser can’t show notifications.');
  if (load('thicket.notify', false)) { save('thicket.notify', false); renderSidebar(); return toast('Notifications off'); }
  const p = await Notification.requestPermission();
  if (p !== 'granted') return toast('Notifications are blocked for this site in your browser settings.');
  save('thicket.notify', true);
  renderSidebar();
  toast('You’ll get a notification when a sprig messages you in the background.');
}

async function signOut() {
  try { await api('/api/logout', { method: 'POST' }); } catch { /* signed out anyway */ }
  endSession();
}

// endSession drops everything the signed-in account loaded and shows the
// signed-out screen. Used for sign-out and for a session that expired or
// was revoked. Requests still in flight compare S.session before writing
// anything, so a late response can't bring the account's data back.
function endSession() {
  es?.close();
  clearTimeout(downTimer);
  save('thicket.layout', null);
  S.session++;
  S.me = null;
  S.panes = []; S.focus = 0; S.details = false; S.filter = '';
  [S.sprigs, S.latest, S.chats, S.typing, S.routines, S.proactive, S.detailsAt, S.drafts].forEach((m) => m.clear());
  S.order = [];
  paneEls.clear();
  $sidebar = $main = null;
  showSignedOut();
}

// ---------- shell ----------

let $sidebar, $main;
const paneEls = new Map();

function renderShell() {
  if (!$sidebar || !$app.contains($sidebar)) {
    $app.className = '';
    $sidebar = h('aside.sidebar', { 'aria-label': 'Sprigs' });
    $main = h('main.main');
    $app.replaceChildren(h('div.shell', {}, $sidebar, $main));
  }
  $app.firstChild.classList.toggle('in-chat', S.panes.length > 0 || !!S.view);
  renderSidebar();
  renderMain();
}

function sprigRow(chat) {
  const room = isRoom(chat);
  const s = room ? null : S.sprigs.get(sprigOf(chat));
  const last = S.latest.get(chat);
  const typing = isTyping(chat);
  const name = room ? 'Everyone' : s.name;
  let preview = room ? 'Talk to any sprig by name' : s.role;
  if (last) preview = (last.role === 'user' ? 'You: ' : room && S.sprigs.get(last.character) ? S.sprigs.get(last.character).name + ': ' : '') + plain(last.content);
  if (typing) preview = (room ? 'Someone' : s.name) + ' is typing…';
  const active = S.panes.includes(chat);
  return h('div.row' + (active ? '.active' : ''), {
    role: 'button', tabindex: 0, 'aria-current': active ? 'true' : null,
    onclick: (e) => openChat(chat, { split: e.shiftKey || e.metaKey || e.ctrlKey }),
    onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openChat(chat, { split: e.shiftKey }); } },
    style: { '--c': s?.color || 'var(--accent)' },
  },
  room ? cluster() : orb(s, 42, { dot: true }),
  h('div.meta', {},
    h('div.top', {}, h('span.name', {}, name), last && h('span.time', {}, relTime(last.at))),
    h('div.preview' + (typing ? '.typing' : ''), {}, preview)),
  unread(chat) && !active && h('span.unread', { 'aria-label': 'unread' }),
  !active && h('button.split', { title: 'Open side by side (or Shift-click)', 'aria-label': `Open ${name} side by side`, onclick: (e) => { e.stopPropagation(); openChat(chat, { split: true }); } }, icon('split')));
}

// desktopBadge shows the unread count on the dock or launcher icon when
// running in the desktop app.
let lastBadge = -1;
function desktopBadge() {
  if (!window.thicketDesktop || !S.me) return;
  const n = [...S.latest.keys()].filter(unread).length;
  if (n !== lastBadge) { lastBadge = n; window.thicketDesktop.setBadge(n); }
}

function renderSidebar() {
  if (!$sidebar || !S.me) return;
  desktopBadge();
  const theme = document.documentElement.dataset.theme || 'auto';
  const q = S.filter.toLowerCase();
  const matches = (id) => {
    if (!q) return true;
    if (id === ROOM) return 'everyone room'.includes(q);
    const s = S.sprigs.get(id);
    const last = S.latest.get(chatOf(id));
    return [s.name, s.full_name, s.role, last?.content || ''].join(' ').toLowerCase().includes(q);
  };
  const ids = S.order.filter(matches);
  // Most recently active first, host breaks ties.
  ids.sort((a, b) => (S.latest.get(chatOf(b))?.id || 0) - (S.latest.get(chatOf(a))?.id || 0));

  const st = S.status;
  const pills = [];
  pills.push(h('span.pill' + (S.online ? '.ok' : '.bad'), {}, h('i'), S.online ? 'Live' : 'Reconnecting'));
  if (st.channels?.discord !== undefined) pills.push(h('span.pill' + (st.channels.discord ? '.ok' : '.warn'), {}, h('i'), 'Discord'));
  const u = S.me.user;
  const admin = u.role === 'owner' || u.role === 'admin';
  if (!st.model_key) {
    pills.push(admin && hostSprig()
      ? h('button.pill.warn.act', { title: 'Sprigs can’t reply until there’s a model key', onclick: askKey }, h('i'), 'No model key: add one')
      : h('span.pill.warn', { title: 'Sprigs can’t reply until the owner adds a model key' }, h('i'), 'No model key'));
  }

  const meter = admin && st.budget_day > 0 && h('div.meter', { title: `Spent this month: $${(st.spend_month || 0).toFixed(2)}${st.budget_month ? ' of $' + st.budget_month : ''}` },
    h('div', { style: { display: 'flex', 'justify-content': 'space-between' } }, h('span', {}, 'Today’s spend'), h('span', {}, `$${(st.spend_day || 0).toFixed(2)} / $${st.budget_day}`)),
    h('div.bar', {}, h('b', { style: { width: Math.min(100, (100 * (st.spend_day || 0)) / st.budget_day) + '%' } })));

  const search = h('input', {
    type: 'search', placeholder: 'Search sprigs and chats', value: S.filter, 'aria-label': 'Search',
    oninput: (e) => { S.filter = e.target.value; renderSidebar(); },
  });
  const had = $sidebar.querySelector('input');
  const refocus = had && document.activeElement === had ? had.selectionStart : null;

  $sidebar.replaceChildren(
    h('div.sb-head', {},
      h('div.brand', {}, h('span.brand-mark', {}, icon('leaf')), 'Thicket'),
      h('button.icon-btn', { title: 'Home', 'aria-label': 'Home', 'aria-pressed': S.panes.length === 0 ? 'true' : 'false', onclick: goHome }, icon('home')),
      h('button.icon-btn', { title: 'Theme', 'aria-label': 'Change theme', onclick: cycleTheme }, icon(theme === 'light' ? 'sun' : theme === 'dark' ? 'moon' : 'auto'))),
    h('label.search', {}, icon('search'), search, h('kbd', {}, '⌘K')),
    !q && h('div.strip', { role: 'list', 'aria-label': 'Quick open' }, S.order.map((id) => {
      const s = S.sprigs.get(id);
      return h('button', { role: 'listitem', title: s.role, onclick: () => openChat(chatOf(id)) }, orb(s, 46), h('span', {}, s.name));
    })),
    h('div.sb-scroll', {},
      !q && memoryRows(),
      !q && householdRows(),
      matches(ROOM) && [h('div.sb-label', {}, 'Rooms'), sprigRow(chatOf(ROOM))],
      ids.length > 0 && [h('div.sb-label', {}, h('span', {}, 'Sprigs'), h('span', {}, String(S.order.length))), ids.map((id) => sprigRow(chatOf(id)))],
      q && !ids.length && !matches(ROOM) && h('div.d-empty', { style: { padding: '12px' } }, 'Nothing matches “' + S.filter + '”.')),
    h('div.sb-foot', {},
      h('div.status-line', {}, pills),
      meter,
      h('div.me', {},
        h('span.me-avatar', { 'aria-hidden': 'true' }, initials(u.name || 'You')),
        h('button.who', { title: 'Your account: name, timezone, password and sessions', onclick: showAccount }, h('b', {}, u.name || 'You'), h('small', {}, { owner: 'Owner', admin: 'Admin', kid: 'Kid' }[u.role] || 'Member')),
        S.me.settings && h('button.icon-btn', { title: 'Settings: models, spending and connectors', 'aria-label': 'Settings', 'aria-pressed': S.view === 'settings' ? 'true' : 'false', onclick: () => openView('settings') }, icon('sliders')),
        h('button.icon-btn', { title: 'Notifications', 'aria-label': 'Toggle notifications', 'aria-pressed': load('thicket.notify', false) ? 'true' : 'false', onclick: toggleNotify }, icon('bell')),
        h('button.icon-btn', { title: 'Account: name, timezone, password and sessions', 'aria-label': 'Account', onclick: showAccount }, icon('key')),
        h('button.icon-btn', { title: 'Sign out', 'aria-label': 'Sign out', onclick: signOut }, icon('out')))));
  if (refocus !== null) { search.focus(); search.setSelectionRange(refocus, refocus); }
}

function renderMain() {
  if (!$main) return;
  const kids = [];
  if (S.view) {
    kids.push(renderView());
  } else if (S.panes.length === 0) {
    kids.push(renderHome());
  } else {
    const wrap = h('div.panes' + (S.panes.length > 1 ? '.multi' : ''));
    S.panes.forEach((chat, i) => {
      const el = paneEls.get(chat) || buildPane(chat);
      el.classList.toggle('focused', i === S.focus);
      wrap.append(el);
    });
    kids.push(wrap);
  }
  if (S.details && S.panes.length) kids.push(h('aside.details', { 'aria-label': 'Details' }));
  // Keep pane nodes (and their scroll and drafts) when re-laying out.
  $main.replaceChildren(...kids);
  for (const chat of [...paneEls.keys()]) if (!S.panes.includes(chat)) paneEls.delete(chat);
  S.panes.forEach((chat) => { updatePaneHead(chat); renderFeed(chat); });
  renderDetails();
}

// ---------- home ----------

function greeting() {
  const hr = new Date().getHours();
  const part = hr < 5 ? 'Up late' : hr < 12 ? 'Good morning' : hr < 18 ? 'Good afternoon' : 'Good evening';
  return part + (S.me.user.name ? ', ' + S.me.user.name.split(' ')[0] : '');
}

// hostSprig is the sprig that does setup in chat (Juni, unless renamed).
function hostSprig() {
  return [...S.sprigs.values()].find((s) => s.host);
}

// askHost opens the host's chat with a request started for the person to
// finish and send. Nothing is sent for them.
async function askHost(text, note) {
  const host = hostSprig();
  if (!host) return toast('There’s no host sprig to ask.');
  const chat = chatOf(host.id);
  if (text) S.drafts.set(chat, text);
  // openChat builds the pane before it waits for the messages. The request
  // goes in now, never after the wait: by then the person may be typing.
  const opened = openChat(chat);
  const ta = paneEls.get(chat)?.querySelector('textarea');
  if (ta) {
    if (text) { ta.value = text; ta.dispatchEvent(new Event('input')); }
    ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length);
  }
  if (note) toast(note);
  await opened;
}

const askKey = () => askHost('', `Paste your OpenRouter key (it starts with sk-or-) to ${hostSprig()?.name || 'the host'}. It’s encrypted, and no model sees it.`);

// What the host sets up in chat, so nobody has to guess that it's asked for.
function setupIdeas() {
  const host = hostSprig();
  const u = S.me.user;
  if (!host || u.role === 'kid') return null;
  const admin = u.role === 'owner' || u.role === 'admin';
  const n = host.name;
  const ideas = [
    admin && !S.status.model_key && ['key', 'Add a model key', 'Sprigs can’t think without one. Paste it in the chat.', askKey],
    ['leaf', 'Bring on a new sprig', 'A helper with its own job, voice and memory.', () => askHost(`${n}, I’d like to bring on a new sprig to help with `)],
    ['calendar', 'Schedule a routine', 'A morning summary, a weekly nudge, a reminder.', () => askHost(`${n}, can you set up a routine that `)],
    admin && ['split', 'Connect a service', 'Home Assistant, Notion, Linear and other tools.', () => askHost(`${n}, can you connect `)],
    S.me.settings && ['sliders', 'Models and spending', 'Change them yourself in Settings.', () => openView('settings')],
  ].filter(Boolean);
  return [
    h('div.section-title', {}, h('h2', {}, `Set things up with ${n}`), h('small', {}, `Setup happens in chat: pick one and finish the sentence`)),
    h('div.ideas', {}, ideas.map(([ic, title, sub, go]) => h('button.idea', { onclick: go }, h('span.view-mark', { 'aria-hidden': 'true' }, icon(ic)), h('div', {}, h('b', {}, title), h('small', {}, sub))))),
  ];
}

function renderHome() {
  const host = S.sprigs.get(S.order[0]);
  const cards = S.order.map((id) => {
    const s = S.sprigs.get(id);
    const last = S.latest.get(chatOf(id));
    return h('button.card', { style: { '--c': s.color }, onclick: (e) => openChat(chatOf(id), { split: e.shiftKey }) },
      h('div.head', {}, orb(s, 44, { dot: true }), h('div', {}, h('b', {}, s.name), h('small', {}, last ? 'Last spoke ' + relTime(last.at) : s.host ? 'Your host' : 'Say hello'))),
      h('p', {}, s.role),
      h('div.foot', {},
        s.traits.slice(0, 2).map((t) => h('span.tag.c', {}, t)),
        s.memories > 0 && h('span.tag', {}, `${s.memories} memories`)));
  });
  const box = composer(chatOf(ROOM), { home: true, placeholder: `Ask anything. Start with a name to pick who answers, e.g. “${host?.name || 'Juni'}, …”` });
  return h('section.home', {},
    h('div.home-scroll', {}, h('div.home-inner', {},
      h('div.hero', {}, h('div.hero-orb', { 'aria-hidden': 'true' }), h('h1', {}, greeting()), h('p', {}, 'Your sprigs remember you, follow up, and keep each other in the loop. Who would you like to talk to?')),
      h('div.section-title', {}, h('h2', {}, 'Your sprigs'), h('small', {}, 'Shift-click to open side by side')),
      h('div.cards', {}, cards),
      setupIdeas())),
    box);
}

// ---------- household: calendar and kid chats ----------

function householdRows() {
  const rows = [];
  if (S.me.calendar) rows.push(viewRow('calendar', 'Calendar', 'Your calendars, sprig jobs and what sprigs added', icon('calendar')));
  for (const k of S.me.parent_of || []) {
    rows.push(viewRow('kid:' + k.id, `${k.name}’s chats`, 'With ' + k.kids.map((x) => x.name).join(', '), orb(k, 42)));
  }
  // Phones have no home screen (the sidebar is home), so the setup ideas
  // get a row of their own there.
  const host = hostSprig();
  if (MOBILE.matches && host && S.me.user.role !== 'kid') rows.push(viewRow('setup', 'Set things up', `Sprigs, routines and services, with ${host.name}`, icon('leaf')));
  return rows.length ? [h('div.sb-label', {}, 'Household'), rows] : null;
}

function viewRow(view, name, sub, mark) {
  const active = S.view === view;
  return h('div.row' + (active ? '.active' : ''), {
    role: 'button', tabindex: 0, 'aria-current': active ? 'true' : null,
    onclick: () => openView(view),
    onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openView(view); } },
  }, h('span.view-mark', { 'aria-hidden': 'true' }, mark), h('div.meta', {}, h('div.top', {}, h('span.name', {}, name)), h('div.preview', {}, sub)));
}

function openView(view) {
  S.view = view;
  S.panes = [];
  S.focus = 0;
  renderShell();
}

function renderView() {
  if (S.view === 'calendar') return renderCalendar();
  if (S.view === 'settings' && S.me.settings) return renderSettings();
  if (S.view === 'setup' && hostSprig()) {
    return h('section.view', { 'aria-label': 'Set things up' },
      viewHead('Set things up', 'Setup happens in chat', h('span.view-mark', { 'aria-hidden': 'true' }, icon('leaf'))),
      h('div.view-scroll', {}, h('div.view-inner', {}, setupIdeas())));
  }
  if (S.view === 'memory') return renderMemory();
  const kid = (S.me.parent_of || []).find((k) => 'kid:' + k.id === S.view);
  if (kid) return renderKidChats(kid);
  return h('section.view', {}, h('div.view-scroll', {}, h('p.d-empty', {}, 'Nothing here.')));
}

function viewHead(title, sub, mark, ...actions) {
  return h('header.pane-head.view-head', {},
    h('button.icon-btn.back', { 'aria-label': 'Back', onclick: goHome }, icon('back')),
    mark, h('div.title', {}, h('b', {}, title), h('small', {}, sub)),
    actions.length > 0 && h('div.actions', {}, actions));
}

// --- calendar ---

const LAYERS = [['events', 'Events'], ['jobs', 'Sprig jobs'], ['outputs', 'Sprig outputs']];

function weekStart(d) {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  x.setDate(x.getDate() - ((x.getDay() + 6) % 7)); // Monday
  return x;
}
const isoDay = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

function renderCalendar() {
  if (!S.cal.start) S.cal.start = weekStart(new Date());
  const start = S.cal.start;
  const end = new Date(start); end.setDate(end.getDate() + 7);
  const last = new Date(end); last.setDate(last.getDate() - 1);
  const range = `${start.toLocaleDateString([], { day: 'numeric', month: 'short' })} – ${last.toLocaleDateString([], { day: 'numeric', month: 'short', year: 'numeric' })}`;
  const move = (days) => { const d = new Date(S.cal.start); d.setDate(d.getDate() + days); S.cal.start = d; renderMain(); };
  const body = h('div.cal-body', {}, h('p.d-empty', {}, 'Loading…'));
  const filters = h('div.cal-filters');
  const el = h('section.view', { 'aria-label': 'Calendar' },
    viewHead('Calendar', range, h('span.view-mark', { 'aria-hidden': 'true' }, icon('calendar')),
      h('button.chip', { onclick: () => move(-7), 'aria-label': 'Previous week' }, '‹'),
      h('button.chip', { onclick: () => { S.cal.start = weekStart(new Date()); renderMain(); } }, 'This week'),
      h('button.chip', { onclick: () => move(7), 'aria-label': 'Next week' }, '›')),
    h('div.view-scroll', {}, h('div.view-inner', {}, filters, body, calendarSources())));
  loadCalendar(start, end, filters, body);
  return el;
}

async function loadCalendar(start, end, filters, body) {
  const session = S.session;
  const layers = LAYERS.map(([k]) => k).filter((k) => S.cal.layers[k] !== false);
  const hide = Object.keys(S.cal.hide).filter((k) => S.cal.hide[k]);
  let res;
  try {
    res = await api(`/api/calendar?from=${isoDay(start)}&to=${isoDay(end)}&layers=${layers.join(',')}&hide=${encodeURIComponent(hide.join(','))}`);
  } catch (e) {
    if (session === S.session) body.replaceChildren(h('p.d-empty', {}, 'Couldn’t load the calendar: ' + e.message));
    return;
  }
  if (session !== S.session || !body.isConnected) return;
  // Layer toggles, then one toggle per sprig.
  const sprigIds = new Set([...Object.keys(res.sprigs || {}), ...Object.keys(S.cal.hide)]);
  const sprigInfo = (id) => res.sprigs?.[id] || S.sprigs.get(id) || { name: id };
  filters.replaceChildren(
    h('div.chips.left', { role: 'group', 'aria-label': 'Layers' }, LAYERS.map(([k, label]) => toggleChip(label, S.cal.layers[k] !== false, () => {
      S.cal.layers = { ...S.cal.layers, [k]: S.cal.layers[k] === false }; save('thicket.cal.layers', S.cal.layers); renderMain();
    }))),
    sprigIds.size > 0 && h('div.chips.left', { role: 'group', 'aria-label': 'Sprigs' }, [...sprigIds].map((id) => {
      const s = sprigInfo(id);
      return toggleChip(s.name, !S.cal.hide[id], () => {
        S.cal.hide = { ...S.cal.hide, [id]: !S.cal.hide[id] }; if (!S.cal.hide[id]) delete S.cal.hide[id];
        save('thicket.cal.hide', S.cal.hide); renderMain();
      }, orb({ ...s, color: s.color || 'var(--accent)' }, 18));
    })));
  const days = [];
  for (let i = 0; i < 7; i++) { const d = new Date(start); d.setDate(d.getDate() + i); days.push(d); }
  const today = isoDay(new Date());
  const byDay = new Map(days.map((d) => [isoDay(d), []]));
  for (const it of res.items) {
    const k = it.all_day ? it.start.slice(0, 10) : isoDay(new Date(it.start));
    byDay.get(k)?.push(it);
  }
  body.replaceChildren(...[
    (res.errors || []).length > 0 && h('div.cal-warn', { role: 'status' }, 'Couldn’t load: ' + res.errors.join('; ')),
    h('div.agenda', {}, days.map((d) => {
      const items = byDay.get(isoDay(d));
      return h('section.ag-day' + (isoDay(d) === today ? '.today' : ''), { 'aria-label': d.toLocaleDateString([], { weekday: 'long', day: 'numeric', month: 'long' }) },
        h('h3', {}, h('span', {}, d.toLocaleDateString([], { weekday: 'short' })), h('b', {}, String(d.getDate()))),
        h('div.ag-items', {}, items.length ? items.map((it) => calItem(it, sprigInfo)) : h('p.d-empty', {}, 'Nothing')));
    }))].filter(Boolean));
}

function toggleChip(label, on, onclick, mark) {
  return h('button.chip.toggle', { 'aria-pressed': on ? 'true' : 'false', onclick }, mark, label);
}

function calItem(it, sprigInfo) {
  const hasEnd = it.end && it.layer !== 'jobs' && new Date(it.end) > new Date(it.start);
  const when = it.all_day ? 'All day' : clock(it.start) + (hasEnd ? '–' + clock(it.end) : '');
  const s = it.sprig ? sprigInfo(it.sprig) : null;
  const meta = [];
  if (it.layer === 'events') meta.push(h('span', {}, it.calendar + (it.shared ? ' · shared' : '')), it.location && h('span', {}, it.location));
  if (it.layer === 'jobs') meta.push(h('span', {}, s?.name || it.sprig), h('span.badge.' + it.status, {}, it.status));
  if (it.layer === 'outputs') {
    meta.push(h('span', {}, 'Added by ' + (s?.name || it.sprig) + (it.shared ? ' · household' : '')));
    if (it.source_url) meta.push(h('a', { href: it.source_url, target: '_blank', rel: 'noopener noreferrer' }, icon('link'), it.source_label || 'Source'));
    else if (it.source_label) meta.push(h('span', {}, 'From ' + it.source_label));
    if (it.removable) meta.push(h('button.ag-remove', { type: 'button', 'aria-label': `Remove ${it.title} from the calendar`, onclick: async (e) => {
      const btn = e.currentTarget; btn.disabled = true;
      try {
        await api('/api/calendar/outputs/' + it.event_id, { method: 'DELETE' });
        toast('Removed. It’s in the trail: put it back from the dashboard’s Changes.'); renderMain();
      } catch (ex) { btn.disabled = false; toast('Couldn’t remove it: ' + ex.message); }
    } }, 'Remove'));
  }
  return h('div.ag-item.' + it.layer, { style: { '--c': s?.color || 'var(--accent)' } },
    h('span.ag-time', {}, when),
    s ? orb({ ...s, color: s.color || 'var(--accent)' }, 26) : h('span.ag-dot', { 'aria-hidden': 'true' }),
    h('div.ag-body', {}, h('b', {}, it.title || '(no title)'), h('div.ag-meta', {}, meta), it.details && h('p', {}, it.details)));
}

function calendarSources() {
  const list = h('div.cal-list', {}, h('p.d-empty', {}, 'Loading…'));
  const load = async () => {
    try {
      const res = await api('/api/calendars');
      list.replaceChildren(...(res.calendars.length ? res.calendars.map((c) => h('div.cal-src', {},
        h('div', {}, h('b', {}, c.name), h('small', {}, `${c.kind === 'caldav' ? 'CalDAV' : 'Calendar feed'}${c.shared ? ' · shared with the household' : ''}${c.mine ? '' : ' · someone else’s'}`),
          c.last_error && h('small.err', {}, c.last_error)),
        c.mine && h('button.chip', { onclick: async () => {
          if (!confirm(`Disconnect ${c.name}? Thicket forgets its address and password.`)) return;
          try { await api('/api/calendars/' + c.id, { method: 'DELETE' }); toast('Disconnected'); load(); renderMain(); } catch (e) { toast(e.message); }
        } }, 'Disconnect'))) : [h('div.cal-empty', {},
          h('p.d-empty', {}, 'No calendars connected yet. Connect yours to see your days next to what your sprigs plan, and give a sprig the calendar to plan around them.'),
          h('button.chip', { type: 'button', onclick: () => { add.open = true; name.focus(); } }, 'Connect your calendar'))]));
    } catch (e) { list.replaceChildren(h('p.d-empty', {}, 'Couldn’t load your calendars: ' + e.message)); }
  };
  load();
  const [nameL, name] = field('Name', { placeholder: 'Work', required: true, maxlength: 60 });
  const kind = h('select.field', { 'aria-label': 'Kind' }, h('option', { value: 'ics' }, 'Calendar feed (ICS or webcal link)'), h('option', { value: 'caldav' }, 'CalDAV (Nextcloud, Fastmail, iCloud)'));
  const [urlL, url] = field('Address', { placeholder: 'https://… or webcal://…', required: true, autocomplete: 'off', spellcheck: 'false' });
  const [userL, user] = field('Username', { autocomplete: 'off' });
  const [pwL, pw] = field('Password (an app password works best)', { type: 'password', autocomplete: 'new-password' });
  const dav = h('div.cal-dav', { hidden: true }, userL, pwL);
  kind.onchange = () => { dav.hidden = kind.value !== 'caldav'; };
  const shared = h('input', { type: 'checkbox' });
  const err = h('p.err', { role: 'alert' });
  const btn = h('button.btn', { type: 'submit' }, 'Connect');
  const form = h('form.cal-form', { onsubmit: async (e) => {
    e.preventDefault(); err.textContent = ''; btn.disabled = true; btn.textContent = 'Checking it loads…';
    try {
      await api('/api/calendars', { method: 'POST', body: { name: name.value.trim(), kind: kind.value, url: url.value.trim(), username: user.value.trim(), password: pw.value, shared: shared.checked } });
      form.reset(); dav.hidden = true; toast('Connected'); load(); renderMain();
    } catch (ex) { err.textContent = ex.message; }
    btn.disabled = false; btn.textContent = 'Connect';
  } }, nameL, h('label.field-label', {}, 'Kind', kind), urlL, dav,
  h('label.check', {}, shared, 'Share with the household'), err, btn,
  h('p.d-empty', {}, 'Read-only: Thicket never changes your calendars. The address and password are stored encrypted.'));
  const add = h('details.cal-add', {}, h('summary', {}, 'Connect a calendar'), form);
  return h('section.cal-sources', {}, h('div.section-title', {}, h('h2', {}, 'Connected calendars')), list, add);
}

// --- settings (owners and admins) ---

const SETTING_GROUPS = [
  ['Models', 'Which models your sprigs think with. Ids are OpenRouter model ids, e.g. anthropic/claude-sonnet-4.5.', ['model', 'fallback_model', 'cheap_model', 'transcribe_model', 'model_url']],
  ['Spending', 'Caps on what models may cost. 0 means no cap.', ['budget.daily_usd', 'budget.monthly_usd']],
  ['Behaviour', '', ['auto_route', 'consistency_check', 'checkin_days', 'history_tokens']],
  ['Web and documents', '', ['web_search', 'search_url', 'pdf_engine', 'document_chars']],
];
const SETTING_LABELS = {
  model: 'Main model', fallback_model: 'Fallback model', cheap_model: 'Small model', transcribe_model: 'Voice transcription model', model_url: 'Model server',
  'budget.daily_usd': 'Daily cap (USD)', 'budget.monthly_usd': 'Monthly cap (USD)',
  auto_route: 'Pick who answers automatically', consistency_check: 'Personality check', checkin_days: 'Check in after quiet days', history_tokens: 'Conversation history (tokens)',
  web_search: 'Web search', search_url: 'SearXNG address', pdf_engine: 'PDF reader', document_chars: 'PDF text kept (characters)',
};
const upFirst = (s) => s.charAt(0).toUpperCase() + s.slice(1);

function settingRow(s) {
  const id = 'set-' + s.name.replace(/\W/g, '-');
  let input;
  if (s.kind === 'bool') {
    input = h('select.field', { id }, h('option', { value: 'true' }, 'On'), h('option', { value: 'false' }, 'Off'));
  } else if (s.kind === 'choice') {
    input = h('select.field', { id }, (s.choices || []).map((c) => h('option', { value: c }, c)));
  } else {
    const num = s.kind === 'int' || s.kind === 'float';
    input = h('input.field', { id, type: num ? 'number' : s.kind === 'url' ? 'url' : 'text', min: num ? 0 : null, step: s.kind === 'float' ? 'any' : null,
      inputmode: s.kind === 'int' ? 'numeric' : s.kind === 'float' ? 'decimal' : null, placeholder: s.default || 'Not set', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false' });
  }
  input.value = s.value;
  const save = h('button.chip', { type: 'submit', hidden: true }, 'Save');
  const reset = h('button.chip', { type: 'button', hidden: !s.changed, title: s.default ? 'Back to ' + s.default : 'Clear it' }, 'Reset');
  const err = h('small.err', { role: 'alert' });
  const dirty = () => { save.hidden = input.value.trim() === s.value; };
  input.addEventListener('input', dirty);
  input.addEventListener('change', dirty);
  const put = async (value) => {
    err.textContent = ''; save.disabled = reset.disabled = true;
    try {
      s = await api('/api/settings', { method: 'POST', body: { name: s.name, value } });
      input.value = s.value; reset.hidden = !s.changed; dirty();
      toast(`${SETTING_LABELS[s.name] || s.name} saved.`);
      if (s.name.startsWith('budget.')) refreshMe().then(renderSidebar).catch(() => {});
    } catch (e) { err.textContent = e.message; }
    save.disabled = reset.disabled = false;
  };
  reset.onclick = () => put('');
  return h('form.set-row', { onsubmit: (e) => { e.preventDefault(); put(input.value.trim()); } },
    h('div.set-text', {}, h('label', { for: id }, SETTING_LABELS[s.name] || s.name), h('small', {}, upFirst(s.help) + '.'), err),
    h('div.set-input', {}, input, save, reset));
}

function settingsList() {
  const body = h('div', {}, h('p.d-empty', {}, 'Loading…'));
  (async () => {
    try {
      const all = (await api('/api/settings')).settings;
      const by = new Map(all.map((s) => [s.name, s]));
      const grouped = new Set(SETTING_GROUPS.flatMap((g) => g[2]));
      const rest = all.filter((s) => !grouped.has(s.name)).map((s) => s.name);
      const groups = rest.length ? [...SETTING_GROUPS, ['Other', '', rest]] : SETTING_GROUPS;
      body.replaceChildren(...groups.map(([title, sub, names]) => h('section.set-group', {},
        h('div.section-title', {}, h('h2', {}, title)), sub && h('p.d-empty', {}, sub),
        names.filter((n) => by.has(n)).map((n) => settingRow(by.get(n))))));
    } catch (e) { body.replaceChildren(h('p.d-empty', {}, 'Couldn’t load the settings: ' + e.message)); }
  })();
  return body;
}

function connectorList() {
  const hostName = hostSprig()?.name || 'Juni';
  const list = h('div.cal-list', {}, h('p.d-empty', {}, 'Loading…'));
  const load = async () => {
    try {
      const items = (await api('/api/integrations')).integrations;
      list.replaceChildren(...(items.length ? items.map((c) => {
        const ok = c.status.startsWith('connected');
        return h('div.cal-src', {},
          h('div', {}, h('b', {}, c.name), h('small', {}, c.target),
            h('small' + (ok ? '' : '.err'), {}, upFirst(c.status)),
            h('small', {}, c.used_by.length ? 'Used by ' + c.used_by.join(', ') : `No sprig can use it yet. ${hostName} gives them access.`)),
          h('button.chip', { title: `Ask ${hostName} which sprigs may use it`, onclick: () => askHost(`${hostName}, give access to the ${c.name} connector to `) }, c.used_by.length ? 'Change access' : 'Give access'),
          h('button.chip', { onclick: async () => {
            if (!confirm(`Remove ${c.name}? Sprigs lose its tools.`)) return;
            try { await api('/api/integrations/' + encodeURIComponent(c.name), { method: 'DELETE' }); toast('Removed'); load(); } catch (e) { toast(e.message); }
          } }, 'Remove'));
      }) : [h('p.d-empty', {}, 'No connectors yet.')]));
    } catch (e) { list.replaceChildren(h('p.d-empty', {}, 'Couldn’t load the connectors: ' + e.message)); }
  };
  load();
  const [nameL, name] = field('Name', { placeholder: 'home_assistant', required: true, pattern: '[a-z0-9]+(_[a-z0-9]+)*', minlength: 2, maxlength: 42, autocapitalize: 'none', spellcheck: 'false', autocomplete: 'off' });
  const [urlL, url] = field('Address of its MCP server', { type: 'url', placeholder: 'https://…/mcp', required: true, autocomplete: 'off', spellcheck: 'false' });
  const [tokL, tok] = field('Access token (if it needs one)', { type: 'password', autocomplete: 'new-password' });
  const err = h('p.err', { role: 'alert' });
  const btn = h('button.btn', { type: 'submit' }, 'Connect');
  const form = h('form.cal-form', { onsubmit: async (e) => {
    e.preventDefault(); err.textContent = ''; btn.disabled = true; btn.textContent = 'Connecting…';
    try {
      const c = await api('/api/integrations', { method: 'POST', body: { name: name.value.trim(), url: url.value.trim(), token: tok.value } });
      form.reset(); toast(`Connected ${c.name}: ${c.tools.length} tool${c.tools.length === 1 ? '' : 's'}.`); load();
    } catch (ex) { err.textContent = upFirst(ex.message); }
    btn.disabled = false; btn.textContent = 'Connect';
  } }, nameL, h('small.hint', {}, 'Lowercase letters, digits and underscores.'), urlL, tokL, err, btn,
  h('p.d-empty', {}, 'The token is stored encrypted and never shown to a model. A new connector’s tools are off for every sprig until you give one access.'));
  const signIn = h('div.cal-src', {},
    h('div', {}, h('b', {}, 'A service you sign in to'), h('small', {}, `Notion, Linear, Sentry and the like have no token to paste. ${hostName} sends you a sign-in link in chat.`)),
    h('button.chip.primary', { onclick: () => askHost(`${hostName}, can you connect `) }, `Ask ${hostName}`));
  return h('section.cal-sources', {}, h('div.section-title', {}, h('h2', {}, 'Connectors')),
    h('p.d-empty', {}, 'Tools from MCP servers you run or point at.'), list,
    h('details.cal-add', {}, h('summary', {}, 'Add a connector by address'), form), signIn);
}

function renderSettings() {
  return h('section.view', { 'aria-label': 'Settings' },
    viewHead('Settings', 'Models, spending and connectors', h('span.view-mark', { 'aria-hidden': 'true' }, icon('sliders'))),
    h('div.view-scroll', {}, h('div.view-inner', {}, settingsList(), connectorList())));
}

// --- kid chats ---

function renderKidChats(kid) {
  const names = kid.kids.map((k) => k.name).join(', ');
  const feed = h('div.kid-feed', {}, h('p.d-empty', {}, 'Loading…'));
  const el = h('section.view', { 'aria-label': `${kid.name}’s chats` },
    viewHead(`${kid.name}’s chats`, `With ${names} · read only`, orb(kid, 38)),
    h('div.view-scroll', {}, h('div.view-inner', {},
      h('div.kid-note', {}, icon('shield'), h('p', {}, `You set up ${kid.name}, so you can read every chat. You won’t get routine notifications, only a safety alert if something worrying comes up. ${names} can’t delete these messages.`)),
      kidAccess(kid),
      h('div.section-title', {}, h('h2', {}, `What ${kid.name} remembers`), h('small', {}, `Stays between ${names} and ${kid.name}`)),
      memoryPanel({ sprig: kid.id, kid }),
      h('div.section-title', {}, h('h2', {}, 'Chats')), feed)));
  loadKidChats(kid, feed, 0, []);
  return el;
}

async function loadKidChats(kid, feed, before, shown) {
  const session = S.session;
  let res;
  try { res = await api(`/api/kid-sprigs/${encodeURIComponent(kid.id)}/chats${before ? '?before=' + before : ''}`); }
  catch (e) { if (session === S.session) feed.replaceChildren(h('p.d-empty', {}, 'Couldn’t load the chats: ' + e.message)); return; }
  if (session !== S.session || !feed.isConnected) return;
  const all = [...res.messages.slice().reverse(), ...shown]; // oldest first
  const kidName = (id) => kid.kids.find((k) => k.id === id)?.name || 'Kid';
  const nodes = [];
  let day = '';
  for (const m of all) {
    const d = dayLabel(new Date(m.at));
    if (d !== day) { day = d; nodes.push(h('div.day', {}, h('span', {}, d))); }
    const mine = m.role === 'user';
    nodes.push(h('div.kid-msg' + (mine ? '.kid' : '.sprig'), {},
      h('div.kid-who', {}, mine ? kidName(m.kid_id) : (S.sprigs.get(m.sprig)?.name || (m.sprig === kid.id ? kid.name : m.sprig)), h('span', {}, clock(m.at))),
      h('div.kid-text', { html: mine ? null : md(m.content) }, mine ? m.content : null)));
  }
  feed.replaceChildren(...[
    res.more && h('button.chip.load-older', { onclick: () => loadKidChats(kid, feed, Math.min(...all.map((m) => m.id)), all) }, 'Load older'),
    ...(nodes.length ? nodes : [h('p.d-empty', {}, `No chats with ${kid.name} yet.`)])].filter(Boolean));
}

function kidAccess(kid) {
  const box = h('section.kid-access');
  const render = () => {
    const tools = kid.tools || [];
    const allow = kid.allow || [];
    const toolInput = h('input.field', { placeholder: 'web, or mcp:<integration>/<tool>', 'aria-label': 'Tool to approve', spellcheck: 'false' });
    const err = h('p.err', { role: 'alert' });
    const change = async (body) => {
      err.textContent = '';
      try {
        const r = await api(`/api/kid-sprigs/${encodeURIComponent(kid.id)}/access`, { method: 'PATCH', body });
        kid.tools = r.tools || []; kid.allow = r.allow || [];
        render(); toast('Saved. It’s in the trail, and you can undo it from the dashboard.');
      } catch (e) { err.textContent = e.message; }
    };
    box.replaceChildren(
      h('div.section-title', {}, h('h2', {}, `What ${kid.name} may use`), h('small', {}, 'Chat only; it never changes anything')),
      h('div.tags', {}, tools.length ? tools.map((t) => h('span.chip.file', {}, t,
        h('button', { type: 'button', 'aria-label': 'Remove ' + t, onclick: () => change({ remove_tools: [t] }) }, icon('x')))) : h('span.d-empty', {}, 'No tools: just chat.')),
      h('form.kid-add', { onsubmit: (e) => { e.preventDefault(); if (toolInput.value.trim()) change({ add_tools: [toolInput.value.trim()] }); } },
        toolInput, h('button.chip.primary', { type: 'submit' }, 'Approve tool')),
      h('div.kid-allow', {},
        [['family_memory', 'Family memory', 'what family sprigs know about the household'], ['calendar', 'Calendar', 'shared household events only'], ['voice', 'Voice', 'it may answer out loud']].map(([k, label, hint]) => {
          const on = allow.includes(k);
          return h('label.check', {}, h('input', { type: 'checkbox', checked: on, onchange: () => change(on ? { remove_allow: [k] } : { add_allow: [k] }) }),
            h('span', {}, h('b', {}, label), ' ', h('small', {}, hint)));
        })),
      err);
  };
  render();
  return box;
}

// --- memory ---

// Relationship kinds first: they're the point of this screen.
const REL_KINDS = ['tone', 'likes', 'dislikes', 'inside_joke', 'shared_history'];
const FACT_KINDS = ['fact', 'preference', 'person', 'project', 'context'];
const KIND_LABELS = { tone: 'How to talk to me', likes: 'Likes', dislikes: 'Dislikes', inside_joke: 'In-joke', shared_history: 'Shared moment',
  fact: 'Fact', preference: 'Preference', person: 'Person', project: 'Project', context: 'For now', followup: 'Follow-up' };

function memoryRows() {
  return [h('div.sb-label', {}, 'You'), viewRow('memory', 'Memory', 'What sprigs remember, and how things are going', icon('memory'))];
}

function renderMemory() {
  const sprig = S.memSprig || '';
  const el = h('section.view', { 'aria-label': 'Memory' },
    viewHead('Memory', 'What your sprigs remember, and how things are going with each', h('span.view-mark', { 'aria-hidden': 'true' }, icon('memory'))),
    h('div.view-scroll', {}, h('div.view-inner', {},
      h('div.chips.left', { role: 'group', 'aria-label': 'Sprig' },
        toggleChip('All sprigs', !sprig, () => { S.memSprig = ''; renderMain(); }),
        S.order.map((id) => { const s = S.sprigs.get(id); return toggleChip(s.name, sprig === id, () => { S.memSprig = id; renderMain(); }, orb(s, 18)); })),
      memoryPanel({ sprig }))));
  return el;
}

// memoryPanel lists memories, relationship apart from facts, each one
// editable, lockable, undoable and deletable. opts.kid: a kid sprig's
// memory, for its parent.
function memoryPanel(opts) {
  const box = h('div.mem-panel', {}, h('p.d-empty', {}, 'Loading…'));
  const q = opts.sprig ? '?sprig=' + encodeURIComponent(opts.sprig) : '';
  const load = async () => {
    const session = S.session;
    let res;
    try { res = await api('/api/memories' + q); }
    catch (e) { if (session === S.session) box.replaceChildren(h('p.d-empty', {}, 'Couldn’t load memories: ' + e.message)); return; }
    if (session !== S.session || !box.isConnected) return;
    const groupBy = (list, key) => {
      const m = new Map();
      for (const x of list) { const k = key(x); if (!m.has(k)) m.set(k, []); m.get(k).push(x); }
      return [...m.entries()].sort((a, b) => (a[0] === '' ? -1 : b[0] === '' ? 1 : a[0].localeCompare(b[0])));
    };
    const section = (title, sub, list, label) => h('section.mem-section', {},
      h('div.section-title', {}, h('h2', {}, title), h('small', {}, sub)),
      list.length ? groupBy(list, (m) => m.sprig_name || '').map(([who, items]) => h('div.mem-group', {},
        h('h3', {}, label(who)), items.map((m) => memoryItem(m, load))))
        : h('p.d-empty', {}, 'Nothing yet.'));
    box.replaceChildren(
      section('Relationship', 'How to be with you: tone, what lands, what annoys', res.relationship,
        (who) => (who ? 'With ' + who : 'Every sprig')),
      section('Facts', 'What they know about you', res.facts, (who) => (who ? 'Just with ' + who : 'Shared with every sprig')),
      memoryAdd(opts, load));
  };
  load();
  return box;
}

function memoryItem(m, reload) {
  const when = new Date(m.created_at);
  const changed = m.updated_at !== m.created_at ? ' · changed ' + relTime(m.updated_at) : '';
  const err = h('p.err', { role: 'alert' });
  const act = async (fn, msg) => {
    err.textContent = '';
    try { await fn(); if (msg) toast(msg); reload(); } catch (e) { err.textContent = e.message; }
  };
  const view = h('div.mem-text', {}, m.content);
  const item = h('div.mem-item' + (m.locked ? '.locked' : ''), {},
    view,
    h('div.mem-meta', {},
      h('span.badge', {}, KIND_LABELS[m.kind] || m.kind),
      m.person_name && h('span.badge', {}, m.person_name),
      m.locked && h('span.badge', { title: 'The automatic memory review never changes it' }, 'Kept as you said'),
      h('span', {}, `From ${m.from} · ${when.toLocaleDateString([], { day: 'numeric', month: 'short', year: 'numeric' })}${changed}`)),
    h('div.mem-actions', {},
      h('button.chip', { onclick: () => edit() }, 'Edit'),
      h('button.chip', { onclick: () => act(() => api(`/api/memories/${m.id}/lock`, { method: 'PUT', body: { locked: !m.locked } }), m.locked ? 'Unlocked: the review may update it' : 'Locked') },
        m.locked ? 'Unlock' : 'Lock'),
      m.undoable && h('button.chip', { onclick: () => act(() => api(`/api/memories/${m.id}/undo`, { method: 'POST' }), 'Undone') }, 'Undo'),
      h('button.chip', { onclick: () => { if (confirm('Forget this for good? It can’t be undone.')) act(() => api(`/api/memories/${m.id}`, { method: 'DELETE' }), 'Forgotten'); } }, 'Delete')),
    err);
  const edit = () => {
    const input = h('textarea.field.mem-input', { rows: 2, maxlength: 300, 'aria-label': 'Memory' });
    input.value = m.content;
    const kinds = REL_KINDS.includes(m.kind) || m.sprig ? [...REL_KINDS, ...FACT_KINDS] : FACT_KINDS;
    const kind = h('select.field.mem-kind', { 'aria-label': 'Kind' }, kinds.concat(kinds.includes(m.kind) ? [] : [m.kind]).map((k) => h('option', { value: k, selected: k === m.kind }, KIND_LABELS[k] || k)));
    const form = h('form.mem-edit', { onsubmit: (e) => {
      e.preventDefault();
      act(() => api(`/api/memories/${m.id}`, { method: 'PATCH', body: { content: input.value.trim(), kind: kind.value } }), 'Saved. Sprigs use it from their next reply.');
    } }, input, h('div.mem-actions', {}, kind, h('button.chip.primary', { type: 'submit' }, 'Save'), h('button.chip', { type: 'button', onclick: reload }, 'Cancel')));
    view.replaceWith(form);
    input.focus();
  };
  return item;
}

function memoryAdd(opts, reload) {
  const sprigs = opts.kid ? [] : S.order.map((id) => S.sprigs.get(id));
  const sprig = !opts.kid && h('select.field.mem-kind', { 'aria-label': 'With' },
    h('option', { value: '' }, 'Every sprig (facts only)'),
    sprigs.map((s) => h('option', { value: s.id, selected: s.id === opts.sprig }, s.name)));
  const person = opts.kid && opts.kid.kids.length > 1 && h('select.field.mem-kind', { 'aria-label': 'About' }, opts.kid.kids.map((k) => h('option', { value: k.id }, k.name)));
  const kind = h('select.field.mem-kind', { 'aria-label': 'Kind' }, [...REL_KINDS, ...FACT_KINDS].map((k) => h('option', { value: k }, KIND_LABELS[k])));
  const input = h('textarea.field.mem-input', { rows: 2, maxlength: 300, required: true, placeholder: 'Prefers short answers, no emoji.', 'aria-label': 'Memory' });
  const err = h('p.err', { role: 'alert' });
  const form = h('form.mem-edit', { onsubmit: async (e) => {
    e.preventDefault(); err.textContent = '';
    try {
      await api('/api/memories', { method: 'POST', body: { sprig: opts.kid ? opts.kid.id : sprig.value, kind: kind.value, content: input.value.trim(), person: person ? Number(person.value) : 0 } });
      toast('Added. Sprigs use it from their next reply.'); reload();
    } catch (ex) { err.textContent = ex.message; }
  } }, input, h('div.mem-actions', {}, sprig, person, kind, h('button.chip.primary', { type: 'submit' }, 'Add')), err,
  h('p.d-empty', {}, 'How to talk to me, likes, dislikes, in-jokes and shared moments are relationship memory: they’re with one sprig. What you write here is kept as you said it.'));
  return h('details.cal-add', {}, h('summary', {}, 'Add a memory'), form);
}

// ---------- panes ----------

function paneMeta(chat) {
  if (isRoom(chat)) return { name: 'Everyone', sub: `${S.order.length} sprigs · address one by name`, color: 'var(--accent)' };
  const s = S.sprigs.get(sprigOf(chat));
  return { name: s.name, sub: s.role, color: s.color, s };
}

function buildPane(chat) {
  const meta = paneMeta(chat);
  const el = h('section.pane', { style: { '--c': meta.color }, 'aria-label': `Chat with ${meta.name}`, onfocusin: () => focusPane(chat), onpointerdown: () => focusPane(chat) },
    h('header.pane-head', {},
      h('button.icon-btn.back', { 'aria-label': 'Back', onclick: goHome }, icon('back')),
      meta.s ? orb(meta.s, 38, { dot: true }) : cluster(),
      h('div.title', {}, h('b', {}, meta.name), h('small', {}, meta.sub)),
      h('div.actions', {},
        h('button.icon-btn', { title: 'Details', 'aria-label': 'Toggle details', onclick: () => { focusPane(chat); toggleDetails(); } }, icon('info')),
        h('button.icon-btn.act-close', { title: 'Close pane', 'aria-label': 'Close pane', onclick: (e) => { e.stopPropagation(); closePane(chat); } }, icon('close')))),
    h('div.feed', {}, h('div.feed-inner')),
    composer(chat));
  paneEls.set(chat, el);
  return el;
}

function focusPane(chat) {
  const i = S.panes.indexOf(chat);
  if (i < 0 || i === S.focus) return;
  S.focus = i;
  paneEls.forEach((el, c) => el.classList.toggle('focused', c === chat));
  persistLayout();
  renderDetails();
}

function updatePaneHead(chat) {
  const el = paneEls.get(chat);
  if (!el) return;
  const small = el.querySelector('.title small');
  const meta = paneMeta(chat);
  const typing = isTyping(chat);
  small.textContent = typing ? (meta.s ? meta.s.name : 'Someone') + ' is typing…' : meta.sub;
  small.classList.toggle('typing', typing);
  el.querySelector('.act-close').style.display = S.panes.length > 1 ? '' : 'none';
}

function suggestions(chat) {
  if (isRoom(chat)) return S.order.slice(0, 3).map((id) => `${S.sprigs.get(id).name}, what can you help me with?`);
  const s = S.sprigs.get(sprigOf(chat));
  const out = ['What can you help me with?', 'What do you remember about me?'];
  if (s.host) out.push('Introduce me to everyone');
  if (s.abilities.includes('Schedules routines')) out.push('Set up a morning summary');
  return out;
}

function renderFeed(chat, animateId, forceBottom) {
  const el = paneEls.get(chat);
  if (!el) return;
  const feed = el.querySelector('.feed');
  const inner = feed.querySelector('.feed-inner');
  const c = S.chats.get(chat);
  const nearBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 120;
  const meta = paneMeta(chat);
  const kids = [];

  if (c?.error && !c.loaded) {
    // No automatic retry: a persistent failure must not loop.
    inner.replaceChildren(h('div.hello', { role: 'alert' },
      h('h2', {}, 'Couldn’t load this chat'), h('p', {}, c.error),
      h('div.chips', { style: { 'margin-top': '10px' } }, h('button.chip.primary', {
        onclick: () => { c.error = ''; renderFeed(chat); },
      }, 'Try again'))));
    return;
  }
  if (!c || !c.loaded) {
    if (!c?.loading) loadChat(chat).then(() => renderFeed(chat, null, true));
    inner.replaceChildren(h('div.hello', {}, h('div.boot-orb')));
    return;
  }
  if (c.more) {
    kids.push(h('button.load-more', {
      onclick: async () => {
        const before = feed.scrollHeight;
        await loadChat(chat, { older: true });
        renderFeed(chat);
        feed.scrollTop = feed.scrollHeight - before;
      },
    }, 'Load earlier messages'));
  }
  if (c.messages.length === 0) {
    kids.push(h('div.hello', {},
      meta.s ? orb(meta.s, 84) : h('div.hero-orb', { style: { width: '84px', height: '84px' } }),
      h('h2', {}, meta.s ? `Say hi to ${meta.s.name}` : 'Everyone’s here'),
      h('p', {}, meta.s ? meta.s.role : 'Start a message with a sprig’s name to choose who answers. Otherwise whoever spoke last picks it up.'),
      h('div.chips', { style: { 'margin-top': '10px' } }, suggestions(chat).map((t) => h('button.chip', { onclick: () => send(chat, t) }, t)))));
  }
  let prev = null, lastDay = '';
  for (const m of c.messages) {
    const d = new Date(m.at);
    const day = d.toDateString();
    if (day !== lastDay) { kids.push(h('div.day', {}, dayLabel(d))); lastDay = day; prev = null; }
    const cont = prev && prev.role === m.role && prev.character === m.character && (d - new Date(prev.at)) < 5 * 60_000;
    kids.push(messageEl(m, cont, m.id === animateId));
    prev = m;
  }
  if (isTyping(chat)) {
    const last = c.messages[c.messages.length - 1];
    const who = meta.s || S.sprigs.get(last?.role === 'assistant' ? last.character : S.order[0]);
    kids.push(h('div.typing-row', { 'aria-label': 'typing' }, orb(who, 30), h('div.dots', {}, h('i'), h('i'), h('i'))));
  }
  inner.replaceChildren(...kids);
  if (!animateId) inner.querySelectorAll('.msg').forEach((n) => { n.style.animation = 'none'; });
  if (nearBottom || forceBottom) feed.scrollTop = feed.scrollHeight;
  renderApproval(chat);
}

function messageEl(m, cont, animate) {
  const actions = !m.pending && !m.failed && m.id > 0 && h('span.msg-actions', {},
    h('button.msg-act' + (m.pinned ? '.on' : ''), {
      title: m.pinned ? 'Unpin' : 'Pin: keep it in view for the rest of this conversation',
      'aria-label': m.pinned ? 'Unpin message' : 'Pin message', 'aria-pressed': m.pinned ? 'true' : 'false',
      onclick: () => togglePin(m),
    }, icon('pin')),
    h('button.msg-act', { title: 'Remember: keep it as a memory, for good', 'aria-label': 'Remember this message', onclick: () => rememberMessage(m) }, icon('brain')),
    h('button.msg-act', { title: 'Delete: remove it here and from what sprigs remember of this conversation', 'aria-label': 'Delete message', onclick: () => deleteMessage(m) }, icon('trash')));
  const pinMark = m.pinned && h('span.pin-mark', { title: 'Pinned for this conversation' }, icon('pin'));
  if (m.role === 'user') {
    const retry = m.failed && h('button', { class: 'stamp', style: { opacity: 1, color: 'var(--err)' }, onclick: () => { const c = S.chats.get(m.chat); c.messages = c.messages.filter((x) => x !== m); send(m.chat, m.text ?? m.content, m.files || []); } }, `Not sent (${m.failed}). Tap to retry`);
    return h('div.msg.user' + (cont ? '.cont' : '') + (m.pending ? '.pending' : '') + (m.failed ? '.failed' : '') + (m.pinned ? '.pinned' : ''), {},
      h('div.stack', {}, h('div.bubble', { html: md(m.content) }), retry || h('span.stamp', {}, actions, pinMark, m.pending ? 'Sending…' : clock(m.at))));
  }
  // m.name is set when the speaker isn't (yet) in the roster: a preview.
  const s = S.sprigs.get(m.character) || { name: m.name || m.character || 'Thicket', color: '#7c8cff' };
  const el = h('div.msg' + (cont ? '.cont' : '') + (m.pinned ? '.pinned' : ''), { style: { '--c': s.color } },
    orb(s, 30),
    h('div.stack', {}, h('span.who', {}, s.name), h('div.bubble', { html: md(m.content) }),
      m.audio && voicePlayer(m, s),
      h('span.stamp', {}, clock(m.at), pinMark, actions)));
  if (!animate) el.style.animation = 'none';
  return el;
}

// Players are kept by message id, so a new message re-rendering the feed
// doesn't cut off one that's playing.
const players = new Map();
function voicePlayer(m, s) {
  let el = players.get(m.id);
  if (!el) {
    el = h('audio.voice', { controls: true, preload: 'none', 'aria-label': `${s.name}, spoken`, src: `/api/chats/${encodeURIComponent(m.chat)}/messages/${m.id}/audio` });
    players.set(m.id, el);
  }
  return el;
}

async function setVoiceMode(s, mode) {
  try {
    const r = await api(`/api/sprigs/${encodeURIComponent(s.id)}/voice`, { method: 'PUT', body: { mode } });
    s.voice_mode = r.voice_mode;
    renderDetails();
    toast({ full: `${s.name} will read replies out in full`, brief: `${s.name} will say a short version; the full text comes too`, off: `${s.name} will only write` }[mode]);
  } catch (e) { toast('Couldn’t change that: ' + e.message); }
}

async function togglePin(m) {
  try {
    const r = await api(`/api/chats/${encodeURIComponent(m.chat)}/messages/${m.id}/pin`, { method: 'POST', body: { pinned: !m.pinned } });
    const changed = r.pinned !== !!m.pinned;
    m.pinned = r.pinned;
    renderFeed(m.chat);
    if (!changed && r.note) toast(r.note);
    else toast(m.pinned ? 'Pinned for the rest of this conversation' : 'Unpinned');
  } catch (e) { toast('Couldn’t change the pin: ' + e.message); }
}

async function rememberMessage(m) {
  try {
    const r = await api(`/api/chats/${encodeURIComponent(m.chat)}/messages/${m.id}/remember`, { method: 'POST' });
    toast(r.note || 'Remembered');
  } catch (e) { toast('Couldn’t remember it: ' + e.message); }
}

async function deleteMessage(m) {
  if (!confirm(m.role === 'user' ? 'Delete your message? Sprigs won’t see it anymore.' : 'Delete this message? Sprigs won’t see it anymore.')) return;
  try {
    await api(`/api/chats/${encodeURIComponent(m.chat)}/messages/${m.id}`, { method: 'DELETE' });
    onDeleted(m.chat, m.id);
  } catch (e) { toast('Couldn’t delete it: ' + e.message); }
}

// Approvals: Thicket asks with "Reply **approve** or **deny**"; offer the
// two answers as buttons.
function renderApproval(chat) {
  const el = paneEls.get(chat);
  const slot = el?.querySelector('.approval-slot');
  if (!slot) return;
  const c = S.chats.get(chat);
  const last = c?.messages[c.messages.length - 1];
  const waiting = last && last.role === 'assistant' && /Reply \*\*approve\*\* or \*\*deny\*\*/.test(last.content);
  slot.replaceChildren(...(waiting ? [h('div.chips', {},
    h('button.chip.primary', { onclick: () => send(chat, 'approve') }, icon('check'), 'Approve'),
    h('button.chip', { onclick: () => send(chat, 'deny') }, icon('x'), 'Deny'))] : []));
}

function composer(chat, opts = {}) {
  const room = isRoom(chat);
  const meta = room ? null : S.sprigs.get(sprigOf(chat));
  const ta = h('textarea', {
    rows: 1,
    placeholder: opts.placeholder || (room ? 'Message everyone' : `Message ${meta.name}`),
    'aria-label': 'Message',
    enterkeyhint: 'send',
  });
  ta.value = S.drafts.get(chat) || '';
  let files = [];
  const btn = h('button.send', { type: 'submit', 'aria-label': 'Send', disabled: !ta.value.trim() }, icon('send'));
  const tray = h('div.tray');
  const showFiles = () => {
    tray.replaceChildren(...files.map((f) => h('span.chip.file', {}, fileLine(f),
      h('button', { type: 'button', 'aria-label': 'Remove ' + f.name, onclick: () => { files = files.filter((x) => x !== f); showFiles(); grow(); } }, icon('x')))));
  };
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 200) + 'px'; btn.disabled = !ta.value.trim() && !files.length; };
  ta.addEventListener('input', () => { S.drafts.set(chat, ta.value); grow(); });
  const submit = async () => {
    const text = ta.value;
    const sending = files;
    if (!text.trim() && !sending.length) return;
    ta.value = ''; S.drafts.delete(chat); files = []; showFiles(); grow();
    if (opts.home) {
      await openChat(chat);
    }
    send(chat, text, sending);
  };
  const addFiles = (list) => {
    const room = MAX_FILES - files.length;
    const add = [...list].slice(0, Math.max(room, 0));
    if (list.length > add.length) toast(`Up to ${MAX_FILES} files per message`);
    files = files.concat(add); showFiles(); grow(); ta.focus();
  };
  const picker = h('input', { type: 'file', multiple: true, hidden: true, accept: 'image/*,audio/*,.pdf,.txt,.md,.csv,.json,.log', onchange: () => { addFiles(picker.files); picker.value = ''; } });
  const attach = h('button.tool', { type: 'button', 'aria-label': 'Attach files', title: 'Attach files', onclick: () => picker.click() }, icon('clip'));
  const mic = canRecord() && recorder(() => ta, (f) => { files = files.concat(f); submit(); });
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !MOBILE.matches) { e.preventDefault(); submit(); }
  });
  queueMicrotask(grow);
  const pick = room && h('div.chips', {}, S.order.map((id) => {
    const s = S.sprigs.get(id);
    return h('button.chip', {
      type: 'button', title: `Ask ${s.name}`,
      onclick: () => {
        // Swap any sprig name already at the start; picking it again clears it.
        const m = ta.value.match(/^\s*([\w-]+),\s*/);
        const named = m && S.order.find((x) => S.sprigs.get(x).name.toLowerCase() === m[1].toLowerCase());
        const rest = named ? ta.value.slice(m[0].length) : ta.value;
        ta.value = named === id ? rest : `${s.name}, ${rest}`;
        S.drafts.set(chat, ta.value); grow(); ta.focus();
      },
    }, orb(s, 18), s.name);
  }));
  return h('form.composer', { onsubmit: (e) => { e.preventDefault(); submit(); } },
    h('div.composer-inner', {},
      h('div.approval-slot'),
      pick,
      tray,
      h('div.box', { ondragover: (e) => e.preventDefault(), ondrop: (e) => { e.preventDefault(); if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files); } },
        picker, attach, ta, mic, btn),
      h('div.hint', {}, 'Enter to send · Shift+Enter for a new line')));
}

const MAX_FILES = 8;
const MAX_RECORDING_MS = 5 * 60 * 1000;

function canRecord() {
  return !!(navigator.mediaDevices?.getUserMedia && window.MediaRecorder);
}

// recorder is the voice message button: tap to record, tap again to send.
// The recording goes up as a file and the server has it transcribed.
function recorder(textarea, done) {
  let rec = null, chunks = [], timer = 0, started = 0;
  const btn = h('button.tool', { type: 'button', 'aria-label': 'Record a voice message', title: 'Record a voice message' }, icon('mic'));
  const reset = () => {
    clearInterval(timer);
    btn.classList.remove('recording');
    btn.replaceChildren(icon('mic'));
    btn.setAttribute('aria-label', 'Record a voice message');
    const ta = textarea(); ta.placeholder = ta.dataset.placeholder || ta.placeholder;
  };
  btn.addEventListener('click', async () => {
    if (rec) { rec.stop(); return; }
    let stream;
    try { stream = await navigator.mediaDevices.getUserMedia({ audio: true }); }
    catch { return toast('Microphone not available. Check the browser’s permission for this site.'); }
    const type = ['audio/webm;codecs=opus', 'audio/ogg;codecs=opus', 'audio/mp4', 'audio/webm'].find((t) => MediaRecorder.isTypeSupported?.(t)) || '';
    try { rec = new MediaRecorder(stream, type ? { mimeType: type } : {}); }
    catch { stream.getTracks().forEach((t) => t.stop()); return toast('This browser can’t record audio.'); }
    chunks = [];
    rec.ondataavailable = (e) => { if (e.data.size) chunks.push(e.data); };
    rec.onstop = () => {
      stream.getTracks().forEach((t) => t.stop());
      const mime = (rec.mimeType || type || 'audio/webm').split(';')[0];
      const ext = mime.includes('mp4') ? 'm4a' : mime.includes('ogg') ? 'ogg' : 'webm';
      const long = Date.now() - started > 700;
      rec = null; reset();
      if (long && chunks.length) done(new File(chunks, `voice message.${ext}`, { type: mime }));
    };
    rec.start();
    started = Date.now();
    btn.classList.add('recording');
    btn.replaceChildren(icon('stop'));
    btn.setAttribute('aria-label', 'Stop and send the voice message');
    const ta = textarea(); ta.dataset.placeholder = ta.placeholder;
    const tick = () => {
      const s = Math.floor((Date.now() - started) / 1000);
      ta.placeholder = `Recording… ${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}. Tap stop to send`;
      if (Date.now() - started > MAX_RECORDING_MS) rec?.stop();
    };
    tick(); timer = setInterval(tick, 500);
  });
  return btn;
}

// ---------- details ----------

// cronText reads the common cron shapes aloud; anything else stays as is.
function cronText(spec) {
  const named = { '@hourly': 'Every hour', '@daily': 'Every day at midnight', '@midnight': 'Every day at midnight', '@weekly': 'Sundays at midnight', '@monthly': 'Monthly' };
  if (named[spec]) return named[spec];
  const f = spec.trim().split(/\s+/);
  if (f.length !== 5 || !/^\d+$/.test(f[0]) || !/^\d+$/.test(f[1]) || f[3] !== '*') return spec;
  const at = new Date(2000, 0, 1, +f[1], +f[0]).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  const days = ['Sundays', 'Mondays', 'Tuesdays', 'Wednesdays', 'Thursdays', 'Fridays', 'Saturdays', 'Sundays'];
  let when;
  if (f[2] !== '*') when = /^\d+$/.test(f[2]) && f[4] === '*' ? `Monthly on day ${f[2]}` : null;
  else if (f[4] === '*') when = 'Every day';
  else if (f[4] === '1-5') when = 'Weekdays';
  else if (f[4] === '0,6' || f[4] === '6,0') when = 'Weekends';
  else if (/^[0-7]$/.test(f[4])) when = days[+f[4]];
  else if (/^[0-7](,[0-7])+$/.test(f[4])) when = f[4].split(',').map((d) => days[+d].slice(0, 3)).join(', ');
  return when ? `${when} at ${at}` : spec;
}

function renderDetails() {
  const el = $main?.querySelector('.details');
  if (!el) return;
  const chat = S.panes[S.focus];
  if (!chat) return;
  const head = h('div.details-head', {}, h('span', {}, 'Details'),
    h('button.icon-btn', { 'aria-label': 'Close details', onclick: () => toggleDetails(false) }, icon('close')));

  if (isRoom(chat)) {
    el.replaceChildren(head, h('div.details-scroll', {},
      h('div.profile', { style: { '--c': 'var(--accent)' } }, h('div.hero-orb', { style: { width: '76px', height: '76px' } }),
        h('h3', {}, 'Everyone'), h('p', {}, 'A shared room. Start with a name (“Pip, …”) and that sprig answers; otherwise whoever spoke last picks it up. Sprigs can hand you over to each other.')),
      h('div.stats', {},
        h('div.stat', {}, h('b', {}, String(S.order.length)), h('small', {}, 'sprigs')),
        h('div.stat', {}, h('b', {}, String(S.me.shared_memories || 0)), h('small', {}, 'shared memories'))),
      h('div.d-section', {}, h('h4', {}, 'In this room'), h('div.d-list', {}, S.order.map((id) => {
        const s = S.sprigs.get(id);
        return h('button.d-item', { style: { width: '100%', 'text-align': 'left' }, onclick: () => openChat(chatOf(id)) },
          orb(s, 26), h('div.txt', {}, h('b', {}, s.name), h('small', {}, s.role)));
      })))));
    return;
  }

  const s = S.sprigs.get(sprigOf(chat));
  // Routines and today's message count change on their own, so the cached
  // copy is shown while a fresh one loads once it's a minute old.
  if (!S.routines.has(s.id)) { S.routines.set(s.id, undefined); loadRoutines(s.id); }
  else if (Date.now() - (S.detailsAt.get(s.id) || 0) > 60000) loadRoutines(s.id);
  const routines = S.routines.get(s.id);
  const routineList = routines === undefined ? h('div.d-empty', {}, 'Loading…')
    : routines === null ? h('div.d-empty', {}, 'Couldn’t load routines.')
      : routines.length === 0 ? h('div.d-empty', {}, s.abilities.includes('Schedules routines') ? `No routines yet. Try “every weekday at 8, summarise my day”.` : `${s.name} doesn’t run routines.`)
        : h('div.d-list', {}, routines.map((r) => {
          const st = !r.enabled ? 'idle' : r.last_status === 'error' || r.last_status === 'missed' ? 'err' : r.last_status ? 'ok' : 'idle';
          const label = !r.enabled ? 'Paused' : r.last_status ? `${{ ok: 'Ran', empty: 'Nothing to report', error: 'Failed', missed: 'Missed', running: 'Running' }[r.last_status] || r.last_status}${r.last_run_at ? ' ' + relTime(r.last_run_at) : ''}` : 'Not run yet';
          return h('div.d-item', {}, h('span.ico.' + st, {}, icon(st === 'ok' ? 'check' : st === 'err' ? 'x' : 'clock')),
            h('div.txt', {}, h('b', {}, r.name), h('small', { title: r.schedule }, `${cronText(r.schedule)} · ${label}`)));
        }));

  const initiative = [];
  if (s.follow_ups) initiative.push('Follows up on things you mention');
  if (s.check_ins) initiative.push('Checks in after a quiet spell');
  if (s.quiet_hours) initiative.push('Quiet hours ' + s.quiet_hours);
  const pro = S.proactive.get(s.id);
  if (pro) {
    initiative.push(`Unprompted messages today: ${pro.sent_today} of ${pro.daily_messages}`);
    for (const m of pro.held || []) {
      initiative.push(`Held: ${m.title} from ${clock(m.at)} (${m.reason === 'budget' ? 'daily limit reached' : 'quiet hours'})`);
    }
  } else if (s.daily_messages) {
    initiative.push(`Up to ${s.daily_messages} unprompted messages a day`);
  }

  el.replaceChildren(head, h('div.details-scroll', {},
    h('div.profile', { style: { '--c': s.color } }, orb(s, 76),
      h('h3', {}, s.name), s.full_name !== s.name && h('small', {}, s.full_name), h('p', {}, s.role), s.catchphrase && h('q', {}, s.catchphrase)),
    h('div.stats', {},
      h('div.stat', {}, h('b', {}, String(s.memories)), h('small', {}, 'own memories')),
      h('div.stat', {}, h('b', {}, String(S.me.shared_memories || 0)), h('small', {}, 'shared with all'))),
    s.traits.length > 0 && h('div.d-section', {}, h('h4', {}, 'Personality'), h('div.tags', {}, s.traits.map((t) => h('span.tag.c', { style: { '--c': s.color } }, t)))),
    h('div.d-section', {}, h('h4', {}, 'Routines'), routineList),
    initiative.length > 0 && h('div.d-section', {}, h('h4', {}, 'Initiative'), h('div.d-list', {}, initiative.map((t) => h('div.d-item', {}, h('span.ico.idle', {}, icon('clock')), h('div.txt', {}, h('small', { style: { color: 'var(--text-2)', 'font-size': '13px' } }, t)))))),
    s.speaks && s.speaks !== 'never' && h('div.d-section', {}, h('h4', {}, 'Voice'),
      h('p.d-empty', {}, s.speaks === 'always' ? `${s.name} answers out loud, with the text too.` : `Send ${s.name} a voice message and you get one back, with the text too.`),
      h('div.chips', { role: 'group', 'aria-label': 'How it speaks to you' }, [['full', 'Read it all'], ['brief', 'Short version'], ['off', 'Text only']].map(([k, label]) =>
        h('button.chip' + (s.voice_mode === k ? '.primary' : ''), { 'aria-pressed': s.voice_mode === k ? 'true' : 'false', onclick: () => setVoiceMode(s, k) }, label)))),
    s.abilities.length > 0 && h('div.d-section', {}, h('h4', {}, 'Abilities'), h('div.tags', {}, s.abilities.map((t) => h('span.tag', {}, t)))),
    h('div.d-section', {}, h('h4', {}, 'Tip'), h('p.d-empty', {}, `Ask ${s.name} “what do you remember about me?” or say “forget that” any time.`),
      h('button.chip', { onclick: () => { S.memSprig = s.id; openView('memory'); } }, `See and change what ${s.name} remembers`))));
}

// ---------- keyboard ----------

document.addEventListener('keydown', (e) => {
  if (!S.me) return;
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    if (MOBILE.matches && S.panes.length) goHome();
    $sidebar?.querySelector('input')?.focus();
  } else if (e.key === 'Escape' && S.details && window.matchMedia('(max-width: 1100px)').matches) {
    toggleDetails(false);
  }
});

MOBILE.addEventListener('change', () => {
  if (MOBILE.matches && S.panes.length > 1) { S.panes = [S.panes[S.focus]]; S.focus = 0; }
  if (S.me) renderShell();
});

// Once a minute: the sidebar, and open details (they refetch once a minute
// old, so held messages going out and a new day show up while they stay open).
setInterval(() => {
  if (!S.me || document.visibilityState !== 'visible') return;
  renderSidebar();
  if (S.details) renderDetails();
}, 60_000);

boot();
