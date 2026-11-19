// Thicket desktop: your Thicket server in its own window, with a tray
// icon, native notifications, microphone access for voice messages, and
// an option to start at login. The server stays where it is (Docker); this
// app only opens it.
const { app, BrowserWindow, Tray, Menu, ipcMain, shell, session, nativeImage, net } = require('electron');
const path = require('node:path');
const { load, save, normalizeServer, sameOrigin } = require('./lib/config');

const PARTITION = 'persist:thicket'; // keeps the sign-in between launches
const icon = nativeImage.createFromPath(path.join(__dirname, 'build', 'icon.png'));

let win = null;
let setupWin = null;
let tray = null;
let quitting = false;
let config = { server: '' };
// Single sign-on leaves the server for the identity provider and comes
// back. While that's under way (from /api/oidc/start, at most 10 minutes)
// the window may visit other https sites; otherwise outside links go to
// the browser.
let signingInUntil = 0;
const SIGN_IN_WINDOW = 10 * 60 * 1000;

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', () => show());
  app.whenReady().then(start);
}

function start() {
  config = load(app.getPath('userData'));
  if (process.platform === 'darwin') app.dock?.setIcon(icon);
  guardSession();
  makeTray();
  if (config.server) openMain(); else openSetup();
}

// Only the configured server gets the microphone and notifications;
// nothing else is granted.
function guardSession() {
  const ses = session.fromPartition(PARTITION);
  const allowed = new Set(['media', 'notifications', 'clipboard-sanitized-write']);
  ses.setPermissionRequestHandler((wc, permission, callback, details) => {
    const ok = allowed.has(permission) && sameOrigin(details.requestingUrl || wc.getURL(), config.server) &&
      (permission !== 'media' || (details.mediaTypes || []).every((t) => t === 'audio'));
    callback(ok);
  });
  ses.setPermissionCheckHandler((wc, permission, origin) => allowed.has(permission) && sameOrigin(origin, config.server));
  ses.webRequest.onCompleted({ urls: ['http://*/api/oidc/start', 'https://*/api/oidc/start'] }, (d) => {
    if (sameOrigin(d.url, config.server) && d.statusCode === 200) signingInUntil = Date.now() + SIGN_IN_WINDOW;
  });
}

// mayNavigate decides whether the window itself may go to url.
function mayNavigate(url) {
  if (sameOrigin(url, config.server)) return true;
  return Date.now() < signingInUntil && url.startsWith('https://');
}

function openMain() {
  if (win) { win.loadURL(config.server); return show(); }
  win = new BrowserWindow({
    width: 1180, height: 820, minWidth: 380, minHeight: 520,
    title: 'Thicket', icon, show: false, backgroundColor: '#0b0e15',
    autoHideMenuBar: true,
    webPreferences: { partition: PARTITION, preload: path.join(__dirname, 'preload.js'), contextIsolation: true, nodeIntegration: false, sandbox: true, spellcheck: true },
  });
  win.once('ready-to-show', () => win.show());
  // Links to anywhere else open in the browser, never in this window.
  win.webContents.setWindowOpenHandler(({ url }) => { openExternal(url); return { action: 'deny' }; });
  win.webContents.on('will-navigate', (e, url) => { if (!mayNavigate(url)) { e.preventDefault(); openExternal(url); } });
  win.webContents.on('did-navigate', (_e, url) => { if (sameOrigin(url, config.server)) signingInUntil = 0; });
  win.webContents.on('did-fail-load', (_e, code, desc, url, isMainFrame) => {
    if (isMainFrame && code !== -3) showOffline(desc); // -3: aborted by a redirect
  });
  win.on('close', (e) => {
    if (!quitting) { e.preventDefault(); win.hide(); } // keeps running in the tray
  });
  win.on('closed', () => { win = null; });
  win.loadURL(config.server);
}

function showOffline(reason) {
  const page = path.join(__dirname, 'setup', 'offline.html');
  win?.loadFile(page, { query: { server: config.server, reason: String(reason || '') } });
}

function openSetup() {
  if (setupWin) return setupWin.focus();
  setupWin = new BrowserWindow({
    width: 480, height: 520, resizable: false, title: 'Connect to Thicket', icon, autoHideMenuBar: true,
    backgroundColor: '#0b0e15',
    webPreferences: { preload: path.join(__dirname, 'preload.js'), contextIsolation: true, nodeIntegration: false, sandbox: true },
  });
  setupWin.loadFile(path.join(__dirname, 'setup', 'setup.html'), { query: { server: config.server } });
  setupWin.on('closed', () => { setupWin = null; if (!config.server && !quitting) app.quit(); });
}

function openExternal(url) {
  try {
    const u = new URL(url);
    if (u.protocol === 'https:' || u.protocol === 'http:' || u.protocol === 'mailto:') shell.openExternal(u.href);
  } catch { /* not a URL: ignore */ }
}

function show() {
  if (!win) return config.server ? openMain() : openSetup();
  if (win.isMinimized()) win.restore();
  win.show();
  win.focus();
}

function makeTray() {
  tray = new Tray(icon.resize({ width: process.platform === 'darwin' ? 18 : 22 }));
  tray.setToolTip('Thicket');
  tray.on('click', () => show());
  refreshTray();
}

function refreshTray() {
  const login = app.getLoginItemSettings().openAtLogin;
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: 'Open Thicket', click: show },
    { type: 'separator' },
    { label: 'Start at login', type: 'checkbox', checked: login, click: (item) => { app.setLoginItemSettings({ openAtLogin: item.checked, openAsHidden: true }); refreshTray(); } },
    { label: 'Change server…', click: openSetup },
    { type: 'separator' },
    { label: 'Quit Thicket', click: () => { quitting = true; app.quit(); } },
  ]));
}

// --- from the pages ---

// Setup calls come only from the app's own pages; the server's pages may
// only bring the window forward and set the badge.
const fromApp = (e) => (e.senderFrame?.url || '').startsWith('file:');
const fromAppOrServer = (e) => fromApp(e) || sameOrigin(e.senderFrame?.url || '', config.server);

ipcMain.handle('thicket:show', (e) => { if (fromAppOrServer(e)) show(); });
ipcMain.handle('thicket:badge', (e, n) => {
  if (!fromAppOrServer(e)) return;
  const count = Math.max(0, Math.min(999, Number(n) || 0));
  app.setBadgeCount(count); // dock (macOS) and Unity launchers
  tray?.setToolTip(count ? `Thicket: ${count} unread` : 'Thicket');
});
ipcMain.handle('thicket:retry', (e) => { if (fromApp(e) && win && config.server) win.loadURL(config.server); });
ipcMain.handle('thicket:change-server', (e) => { if (fromApp(e)) openSetup(); });
ipcMain.handle('thicket:connect', async (e, input) => {
  if (!fromApp(e)) return { ok: false, error: 'not allowed' };
  let server;
  try { server = normalizeServer(input); } catch (err) { return { ok: false, error: err.message }; }
  try {
    const res = await net.fetch(server + '/health', { signal: AbortSignal.timeout(8000) });
    if (res.status !== 200 && res.status !== 503) return { ok: false, error: `${server} answered, but doesn’t look like Thicket (HTTP ${res.status}).` };
    const body = await res.json().catch(() => ({}));
    if (!body || typeof body.status !== 'string' || !('checks' in body)) return { ok: false, error: `${server} answered, but doesn’t look like Thicket.` };
  } catch (err) {
    return { ok: false, error: `Couldn’t reach ${server}: ${err.message}` };
  }
  const changed = server !== config.server;
  config = { ...config, server };
  save(app.getPath('userData'), config);
  setupWin?.close();
  if (changed && win) win.loadURL(server); else openMain();
  return { ok: true };
});

app.on('activate', () => show()); // macOS dock click
app.on('before-quit', () => { quitting = true; });
app.on('window-all-closed', () => { /* stay in the tray */ });
