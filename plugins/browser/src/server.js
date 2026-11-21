'use strict';
// The plugin's one port: GET /health (no token), POST /mcp (the tools)
// and /v1/ (what Thicket core alone uses: the doctor check, open
// browsers, and the sealed profiles for `thicket backup`). Everything but
// /health needs the token Thicket shares through a file.

const http = require('node:http');
const crypto = require('node:crypto');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const tools = require('./tools');
const vault = require('./vault');
const { Sessions, chromiumArgs } = require('./sessions');
const { EgressProxy, Blocked } = require('./proxy');

function readToken(file) {
  try {
    const t = fs.readFileSync(file, 'utf8').trim();
    return t.length >= 32 ? t : null;
  } catch {
    return null;
  }
}

// authorized compares in constant time (hashes, so lengths don't leak),
// reading the file on every request.
function authorized(req, tokenFile) {
  const want = readToken(tokenFile);
  if (!want) return false;
  const h = String(req.headers.authorization || '');
  if (!h.startsWith('Bearer ')) return false;
  const a = crypto.createHash('sha256').update(h.slice(7)).digest();
  const b = crypto.createHash('sha256').update(want).digest();
  return crypto.timingSafeEqual(a, b);
}

function readBody(req, max) {
  return new Promise((resolve, reject) => {
    const parts = [];
    let n = 0;
    req.on('data', (c) => {
      n += c.length;
      if (n > max) {
        reject(Object.assign(new Error('too large'), { status: 413 }));
        req.destroy();
        return;
      }
      parts.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(parts)));
    req.on('error', reject);
  });
}

function json(res, status, v) {
  const b = JSON.stringify(v);
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(b) });
  res.end(b);
}

// doctor: Chromium starts, and its proxy refuses a host that isn't on
// the allowlist and a private address that is.
async function doctor(opts) {
  const checks = [];
  const blocks = [];
  const proxy = new EgressProxy({ allow: ['allowed.invalid', '169.254.169.254', 'localhost'], onBlock: (e) => blocks.push(e), lookup: opts.lookup });
  let browser;
  try {
    await proxy.listen();
    const t0 = Date.now();
    browser = await opts.chromium.launch({ headless: true, args: chromiumArgs(proxy.port) });
    checks.push({ name: 'chromium', ok: true, detail: `Chromium ${browser.version()} started in ${Date.now() - t0} ms` });
    const page = await browser.newPage();
    let resp = null;
    try {
      resp = await page.goto('http://not-on-the-list.invalid/', { timeout: 15000 });
    } catch {
      // a refused CONNECT or 403 is what we want
    }
    const ok = blocks.some((b) => b.host === 'not-on-the-list.invalid' && b.kind === 'allowlist') && (!resp || resp.status() === 403);
    checks.push({ name: 'proxy_blocks_unlisted', ok, detail: ok ? 'a host not on the allowlist was refused by the proxy' : 'a host not on the allowlist was NOT refused' });
  } catch (e) {
    checks.push({ name: 'chromium', ok: false, detail: String(e.message).split('\n')[0] });
  } finally {
    if (browser) await browser.close().catch(() => {});
  }
  for (const [host, label] of [['169.254.169.254', 'cloud metadata'], ['localhost', 'loopback']]) {
    try {
      await proxy.check(host, 80);
      checks.push({ name: 'proxy_blocks_private', ok: false, detail: `${label} (${host}) was NOT refused` });
    } catch (e) {
      const ok = e instanceof Blocked && (e.kind === 'private' || e.kind === 'dns');
      checks.push({ name: 'proxy_blocks_private', ok, detail: ok ? `${label} (${host}) refused even when on the allowlist` : String(e.message) });
    }
  }
  await proxy.close();
  return { checks, open: opts.sessions.list(), max: opts.sessions.max };
}

function createServer(opts) {
  const { tokenFile, sessions, version, log = () => {} } = opts;
  const profileMax = opts.maxProfileUploadBytes || 512 * 1048576;

  async function mcp(req, res) {
    let msg;
    try {
      msg = JSON.parse((await readBody(req, 1 << 20)).toString('utf8'));
    } catch {
      res.writeHead(400).end('bad request');
      return;
    }
    if (msg.id === undefined || msg.id === null) {
      res.writeHead(202).end();
      return;
    }
    const reply = (result, error) => json(res, 200, error ? { jsonrpc: '2.0', id: msg.id, error } : { jsonrpc: '2.0', id: msg.id, result });
    const p = msg.params || {};
    switch (msg.method) {
      case 'initialize':
        return reply({
          protocolVersion: typeof p.protocolVersion === 'string' ? p.protocolVersion : '2025-06-18',
          capabilities: { tools: {} },
          serverInfo: { name: 'thicket-browser', version },
        });
      case 'ping':
        return reply({});
      case 'tools/list':
        return reply({ tools: tools.list() });
      case 'tools/call': {
        const result = await tools.call(sessions, p.name, p.arguments, p._meta);
        return reply(result);
      }
      default:
        return reply(null, { code: -32601, message: `method ${msg.method} not found` });
    }
  }

  async function admin(req, res, url) {
    const parts = url.pathname.split('/').filter(Boolean); // v1, ...
    if (req.method === 'GET' && parts[1] === 'doctor' && parts.length === 2) {
      return json(res, 200, await doctor({ chromium: opts.chromium, sessions, lookup: opts.lookup }));
    }
    if (req.method === 'GET' && parts[1] === 'sessions' && parts.length === 2) {
      return json(res, 200, { open: sessions.list(), max: sessions.max });
    }
    if (parts[1] === 'profiles' && parts.length === 2 && req.method === 'GET') {
      const out = [];
      for (const f of await fsp.readdir(sessions.profilesDir).catch(() => [])) {
        const m = /^(.+)\.tbp$/.exec(f);
        if (!m || !Sessions.validSprig(m[1])) continue;
        const st = await fsp.stat(sessions.profileFile(m[1])).catch(() => null);
        if (st) out.push({ sprig: m[1], size: st.size, modified: st.mtime.toISOString(), open: sessions.isOpen(m[1]) });
      }
      return json(res, 200, { profiles: out });
    }
    if (parts[1] === 'profiles' && parts.length === 3) {
      const sprig = decodeURIComponent(parts[2]);
      if (!Sessions.validSprig(sprig)) return json(res, 400, { error: 'not a sprig name' });
      const file = sessions.profileFile(sprig);
      if (req.method === 'GET') {
        // The sealed file as it is at rest: a browser open right now
        // isn't in it until it closes.
        let b;
        try {
          b = await fsp.readFile(file);
        } catch {
          return json(res, 404, { error: 'no saved profile' });
        }
        res.writeHead(200, { 'content-type': 'application/octet-stream', 'content-length': b.length, 'x-browser-open': String(sessions.isOpen(sprig)) });
        return res.end(b);
      }
      if (req.method === 'PUT') {
        if (sessions.isOpen(sprig)) return json(res, 409, { error: 'that sprig\'s browser is open; it can be replaced once it closes' });
        let b;
        try {
          b = await readBody(req, profileMax);
        } catch (e) {
          return json(res, e.status || 400, { error: e.message });
        }
        if (!vault.looksSealed(b)) return json(res, 400, { error: 'not a sealed browser profile' });
        const tmp = file + '.tmp-' + process.pid;
        await fsp.writeFile(tmp, b, { mode: 0o600 });
        await fsp.rename(tmp, file);
        log('profile replaced', sprig);
        return json(res, 200, {});
      }
      if (req.method === 'DELETE') {
        await sessions.discard(sprig);
        log('profile deleted', sprig);
        return json(res, 200, {});
      }
    }
    return json(res, 404, { error: 'not found' });
  }

  return http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://plugin');
      if (req.method === 'GET' && url.pathname === '/health') {
        res.writeHead(200, { 'content-type': 'text/plain' });
        return res.end('ok\n');
      }
      if (!authorized(req, tokenFile)) {
        res.writeHead(401, { 'content-type': 'text/plain' });
        return res.end('unauthorized');
      }
      if (req.method === 'POST' && url.pathname === '/mcp') return await mcp(req, res);
      if (url.pathname.startsWith('/v1/')) return await admin(req, res, url);
      res.writeHead(404).end();
    } catch (e) {
      log('request failed', e.message);
      if (!res.headersSent) json(res, 500, { error: 'internal error' });
      else res.end();
    }
  });
}

module.exports = { createServer, doctor, authorized };
