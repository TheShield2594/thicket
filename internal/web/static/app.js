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
  status: {},
  panes: [],           // open chat ids, left to right
  focus: 0,
  details: false,
  filter: '',
  drafts: new Map(),
  lastRead: load('thicket.read', {}),
  online: true,
  session: 0,          // bumped on sign-out; stale responses are dropped
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
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, init);
  let data = {};
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
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
  history.replaceState(null, '', '/login'); // keep the token out of history
  if (!token) return showSignedOut();
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

function showSignedOut() {
  showGate('Sign in to Thicket', 'Sign-in works with a one-time link. Get one either way:',
    h('div.steps', {},
      h('div', {}, h('b', {}, '1'), h('span', {}, 'In a Discord DM, ask Juni for a “web app link”.')),
      h('div', {}, h('b', {}, '2'), h('span', {}, 'Or on the server run ', h('code', {}, 'thicket web link'), '.'))));
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
      try { await api('/api/me'); } catch (e) { if (e.status === 401) { es.close(); showSignedOut(); } }
    }, 4000);
  };
  es.onmessage = (e) => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    if (ev.type === 'message') onMessage(ev.chat, ev.message);
    else if (ev.type === 'typing') onTyping(ev.chat);
    else if (ev.type === 'deleted') onDeleted(ev.chat, ev.id);
  };
}

async function resync() {
  try { await refreshMe(); } catch { return; }
  for (const chat of S.panes) await loadChat(chat, { fresh: true });
  if (S.me) renderShell();
}

function onMessage(chat, m) {
  const c = S.chats.get(chat);
  if (c?.loaded && !c.messages.some((x) => x.id === m.id)) {
    // Replace the optimistic copy of our own message if it's still there.
    const i = m.role === 'user' ? c.messages.findIndex((x) => x.pending && x.content === m.content) : -1;
    if (i >= 0) c.messages.splice(i, 1, m); else c.messages.push(m);
  }
  if (m.role === 'assistant') {
    S.typing.delete(chat);
    notify(chat, m);
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
    n.onclick = () => { window.focus(); openChat(chat); n.close(); };
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
  try {
    const before = opts.older && c.messages.length ? c.messages[0].id : 0;
    const res = await api(`/api/chats/${encodeURIComponent(chat)}/messages${before ? '?before=' + before : ''}`);
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
    if (!c.loaded) c.error = e.message || 'network error';
    else toast('Couldn’t refresh messages: ' + e.message);
  } finally {
    c.loading = false;
  }
}

async function loadRoutines(id) {
  try {
    const res = await api('/api/sprigs/' + encodeURIComponent(id));
    S.routines.set(id, res.routines);
  } catch { S.routines.set(id, null); }
  renderDetails();
}

let tempId = -1;
async function send(chat, text) {
  text = text.trim();
  if (!text) return;
  const c = S.chats.get(chat);
  const m = { id: tempId--, chat, role: 'user', content: text, at: new Date().toISOString(), pending: true };
  c.messages.push(m);
  renderFeed(chat, m.id, true);
  try {
    const res = await api(`/api/chats/${encodeURIComponent(chat)}/messages`, { method: 'POST', body: { text } });
    const i = c.messages.indexOf(m);
    const dup = c.messages.some((x) => x.id === res.message.id);
    if (i >= 0) { if (dup) c.messages.splice(i, 1); else c.messages.splice(i, 1, res.message); }
    if ((S.latest.get(chat)?.id || 0) < res.message.id) S.latest.set(chat, res.message);
    renderSidebar();
  } catch (e) {
    m.pending = false; m.failed = e.message || 'Not sent';
  }
  renderFeed(chat);
}

// ---------- layout actions ----------

async function openChat(chat, opts = {}) {
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
  es?.close();
  clearTimeout(downTimer);
  save('thicket.layout', null);
  // Drop everything the previous account loaded. Pending requests check
  // S.me (and the cleared pane nodes) before touching the screen.
  S.session++;
  S.me = null;
  S.panes = []; S.focus = 0; S.details = false; S.filter = '';
  [S.sprigs, S.latest, S.chats, S.typing, S.routines, S.drafts].forEach((m) => m.clear());
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
  $app.firstChild.classList.toggle('in-chat', S.panes.length > 0);
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

function renderSidebar() {
  if (!$sidebar || !S.me) return;
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
  if (!st.model_key) pills.push(h('span.pill.warn', { title: 'Paste an OpenRouter key to Juni to get started' }, h('i'), 'No model key'));

  const u = S.me.user;
  const admin = u.role === 'owner' || u.role === 'admin';
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
      matches(ROOM) && [h('div.sb-label', {}, 'Rooms'), sprigRow(chatOf(ROOM))],
      ids.length > 0 && [h('div.sb-label', {}, h('span', {}, 'Sprigs'), h('span', {}, String(S.order.length))), ids.map((id) => sprigRow(chatOf(id)))],
      q && !ids.length && !matches(ROOM) && h('div.d-empty', { style: { padding: '12px' } }, 'Nothing matches “' + S.filter + '”.')),
    h('div.sb-foot', {},
      h('div.status-line', {}, pills),
      meter,
      h('div.me', {},
        h('span.me-avatar', { 'aria-hidden': 'true' }, initials(u.name || 'You')),
        h('div.who', {}, h('b', {}, u.name || 'You'), h('small', {}, u.role === 'owner' ? 'Owner' : u.role === 'admin' ? 'Admin' : 'Member')),
        h('button.icon-btn', { title: 'Notifications', 'aria-label': 'Toggle notifications', 'aria-pressed': load('thicket.notify', false) ? 'true' : 'false', onclick: toggleNotify }, icon('bell')),
        h('button.icon-btn', { title: 'Sign out', 'aria-label': 'Sign out', onclick: signOut }, icon('out')))));
  if (refocus !== null) { search.focus(); search.setSelectionRange(refocus, refocus); }
}

function renderMain() {
  if (!$main) return;
  const kids = [];
  if (S.panes.length === 0) {
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
      h('div.cards', {}, cards))),
    box);
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
  if (m.role === 'user') {
    const retry = m.failed && h('button', { class: 'stamp', style: { opacity: 1, color: 'var(--err)' }, onclick: () => { const c = S.chats.get(m.chat); c.messages = c.messages.filter((x) => x !== m); send(m.chat, m.content); } }, `Not sent (${m.failed}). Tap to retry`);
    return h('div.msg.user' + (cont ? '.cont' : '') + (m.pending ? '.pending' : '') + (m.failed ? '.failed' : ''), {},
      h('div.stack', {}, h('div.bubble', { html: md(m.content) }), retry || h('span.stamp', {}, m.pending ? 'Sending…' : clock(m.at))));
  }
  const s = S.sprigs.get(m.character) || { name: m.character || 'Thicket', color: '#7c8cff' };
  const el = h('div.msg' + (cont ? '.cont' : ''), { style: { '--c': s.color } },
    orb(s, 30),
    h('div.stack', {}, h('span.who', {}, s.name), h('div.bubble', { html: md(m.content) }), h('span.stamp', {}, clock(m.at))));
  if (!animate) el.style.animation = 'none';
  return el;
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
  const btn = h('button.send', { type: 'submit', 'aria-label': 'Send', disabled: !ta.value.trim() }, icon('send'));
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 200) + 'px'; btn.disabled = !ta.value.trim(); };
  ta.addEventListener('input', () => { S.drafts.set(chat, ta.value); grow(); });
  const submit = async () => {
    const text = ta.value;
    if (!text.trim()) return;
    ta.value = ''; S.drafts.delete(chat); grow();
    if (opts.home) {
      await openChat(chat);
    }
    send(chat, text);
  };
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
      h('div.box', {}, ta, btn),
      h('div.hint', {}, 'Enter to send · Shift+Enter for a new line')));
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
  if (!S.routines.has(s.id)) { S.routines.set(s.id, undefined); loadRoutines(s.id); }
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

  el.replaceChildren(head, h('div.details-scroll', {},
    h('div.profile', { style: { '--c': s.color } }, orb(s, 76),
      h('h3', {}, s.name), s.full_name !== s.name && h('small', {}, s.full_name), h('p', {}, s.role), s.catchphrase && h('q', {}, s.catchphrase)),
    h('div.stats', {},
      h('div.stat', {}, h('b', {}, String(s.memories)), h('small', {}, 'own memories')),
      h('div.stat', {}, h('b', {}, String(S.me.shared_memories || 0)), h('small', {}, 'shared with all'))),
    s.traits.length > 0 && h('div.d-section', {}, h('h4', {}, 'Personality'), h('div.tags', {}, s.traits.map((t) => h('span.tag.c', { style: { '--c': s.color } }, t)))),
    h('div.d-section', {}, h('h4', {}, 'Routines'), routineList),
    initiative.length > 0 && h('div.d-section', {}, h('h4', {}, 'Initiative'), h('div.d-list', {}, initiative.map((t) => h('div.d-item', {}, h('span.ico.idle', {}, icon('clock')), h('div.txt', {}, h('small', { style: { color: 'var(--text-2)', 'font-size': '13px' } }, t)))))),
    s.abilities.length > 0 && h('div.d-section', {}, h('h4', {}, 'Abilities'), h('div.tags', {}, s.abilities.map((t) => h('span.tag', {}, t)))),
    h('div.d-section', {}, h('h4', {}, 'Tip'), h('p.d-empty', {}, `Ask ${s.name} “what do you remember about me?” or say “forget that” any time.`))));
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

setInterval(() => { if (S.me && document.visibilityState === 'visible') renderSidebar(); }, 60_000);

boot();
