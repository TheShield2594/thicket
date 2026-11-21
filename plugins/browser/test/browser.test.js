'use strict';
// End to end, with a real Chromium: a sprig logs in to an allowlisted
// test site, the login survives a restart of the plugin, and requests to
// hosts off the allowlist, to Thicket, the Docker host, cloud metadata
// and the LAN are refused by the proxy.
const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require('playwright');
const { Sessions } = require('../src/sessions');
const { createServer } = require('../src/server');

const TOKEN = 'x'.repeat(40);

function testSite() {
  const hits = [];
  const uploads = [];
  const server = http.createServer((req, res) => {
    hits.push({ host: req.headers.host, path: req.url });
    const port = server.address().port;
    const cookie = req.headers.cookie || '';
    const html = (s, status = 200, extra = {}) => {
      res.writeHead(status, { 'content-type': 'text/html; charset=utf-8', ...extra });
      res.end(`<!doctype html><html><head><title>Test shop</title></head><body>${s}</body></html>`);
    };
    if (req.url === '/' || req.url === '/login-page') {
      return html(`<h1>Sign in</h1>
        <form method="post" action="/login">
          <label>Username <input name="user"></label>
          <label>Password <input name="pass" type="password"></label>
          <button>Sign in</button>
        </form>
        <img src="http://tracker.test:${port}/pixel.gif" alt="">
        <script>
          fetch('http://127.0.0.1:${port}/direct').catch(() => {});
          fetch('http://thicket:8080/api').catch(() => {});
        </script>`);
    }
    if (req.url === '/login' && req.method === 'POST') {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        const p = new URLSearchParams(body);
        if (p.get('user') === 'pip' && p.get('pass') === 'hunter2') {
          res.writeHead(302, { location: '/account', 'set-cookie': 'sid=s3cr3t-session; Max-Age=86400; Path=/; HttpOnly' });
          return res.end();
        }
        html('<p>Wrong password</p>', 401);
      });
      return;
    }
    if (req.url === '/account') {
      if (!cookie.includes('sid=s3cr3t-session')) return html('<p>Please sign in</p><a href="/">Sign in</a>');
      return html(`<h1>Welcome back, pip</h1>
        <a href="/receipt.txt">Get receipt</a>
        <a href="/big.bin">Get big file</a>
        <a href="http://evil.test:${port}/">Leave</a>
        <a href="/go-evil">Redirect away</a>
        <form method="post" action="/upload" enctype="multipart/form-data">
          <label>Attach <input type="file" name="f"></label><button>Upload</button>
        </form>
        <select name="size"><option value="s">Small</option><option value="l">Large</option></select>`);
    }
    if (req.url === '/receipt.txt') {
      res.writeHead(200, { 'content-type': 'text/plain', 'content-disposition': 'attachment; filename="receipt.txt"' });
      return res.end('receipt #42\n');
    }
    if (req.url === '/big.bin') {
      res.writeHead(200, { 'content-type': 'application/octet-stream', 'content-disposition': 'attachment; filename="big.bin"' });
      return res.end(Buffer.alloc(3 * 1048576));
    }
    if (req.url === '/go-evil') {
      res.writeHead(302, { location: `http://evil.test:${port}/` });
      return res.end();
    }
    if (req.url === '/upload' && req.method === 'POST') {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        const m = /filename="([^"]*)"/.exec(body);
        uploads.push(m ? m[1] : '');
        html(`<p>Uploaded ${m ? m[1] : 'nothing'}</p>`);
      });
      return;
    }
    if (req.url === '/pay') return html('<label>Card <input autocomplete="cc-number" name="cc"></label>');
    res.writeHead(404).end();
  });
  return { server, hits, uploads };
}

function lookupFor() {
  const table = {
    'login.test': ['127.0.0.1'], 'evil.test': ['127.0.0.1'], 'tracker.test': ['127.0.0.1'],
    thicket: ['172.18.0.2'], 'host.docker.internal': ['172.17.0.1'], 'nas.lan': ['10.0.0.1'],
  };
  return async (h) => {
    if (!table[h]) throw Object.assign(new Error('not found'), { code: 'ENOTFOUND' });
    return table[h];
  };
}

async function harness(t, opts = {}) {
  const site = testSite();
  await new Promise((r) => site.server.listen(0, '127.0.0.1', r));
  const port = site.server.address().port;
  const dir = await fsp.mkdtemp(path.join(os.tmpdir(), 'tb-e2e-'));
  const tokenFile = path.join(dir, 'token');
  await fsp.writeFile(tokenFile, TOKEN + '\n');
  const h = { site, port, dir, tokenFile, lookup: lookupFor() };
  h.start = async () => {
    h.sessions = new Sessions({
      chromium,
      profilesDir: path.join(dir, 'profiles'),
      runDir: path.join(dir, 'run'),
      max: opts.max || 2,
      idleMs: opts.idleMs || 60000,
      fileCapBytes: 1048576,
      downloadsCapBytes: 2 * 1048576,
      proxy: { lookup: h.lookup, unsafeLoopbackPorts: [port] },
    });
    await h.sessions.init();
    h.server = createServer({ tokenFile, sessions: h.sessions, chromium, version: 'test', lookup: h.lookup });
    await new Promise((r) => h.server.listen(0, '127.0.0.1', r));
    h.url = `http://127.0.0.1:${h.server.address().port}`;
  };
  h.stop = async () => {
    await h.sessions.closeAll();
    await new Promise((r) => h.server.close(r));
  };
  h.rpc = async (method, params, token = TOKEN) => {
    const r = await fetch(h.url + '/mcp', {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: 'Bearer ' + token },
      body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
    });
    if (r.status !== 200) return { status: r.status };
    return (await r.json()).result;
  };
  h.keys = {};
  h.call = async (sprig, name, args = {}, allow = ['login.test', 'thicket', 'host.docker.internal', '169.254.169.254', 'nas.lan']) => {
    h.keys[sprig] = h.keys[sprig] || crypto.randomBytes(32).toString('base64');
    const res = await h.rpc('tools/call', {
      name, arguments: args,
      _meta: { 'thicket/call': { sprig, approved: false, draft_only: false }, 'thicket/browser': { allow, profile_key: h.keys[sprig] } },
    });
    res.text = res.content.filter((c) => c.type === 'text').map((c) => c.text).join('\n');
    if (!res.isError && /^\[\d+\]/m.test(res.text)) h.last = res.text; // the last page reading
    return res;
  };
  await h.start();
  t.after(async () => {
    await h.stop().catch(() => {});
    site.server.close();
    await fsp.rm(dir, { recursive: true, force: true });
  });
  return h;
}

test('a sprig logs in, the login survives a restart, and the proxy blocks what it must', { timeout: 120000 }, async (t) => {
  const h = await harness(t);
  const base = `http://login.test:${h.port}`;

  // Token and tool list.
  assert.equal((await h.rpc('tools/list', {}, 'wrong')).status, 401);
  const list = await h.rpc('tools/list', {});
  assert.deepEqual(list.tools.map((x) => x.name).sort(), ['back', 'click', 'done', 'open', 'read_page', 'screenshot', 'select', 'type']);
  assert.equal(list.tools.find((x) => x.name === 'click').annotations.readOnlyHint, false);
  assert.equal(list.tools.find((x) => x.name === 'read_page').annotations.readOnlyHint, true);

  // No sprig in _meta: refused.
  const anon = await h.rpc('tools/call', { name: 'open', arguments: { url: base } });
  assert.equal(anon.isError, true);

  // Log in.
  let r = await h.call('pip', 'open', { url: base + '/' });
  assert.ok(!r.isError, r.text);
  assert.match(r.text, /untrusted content from login\.test/);
  assert.match(r.text, /Blocked while loading.*tracker\.test \(not on the allowlist\)/s);
  assert.match(r.text, /thicket \(private address\)/);
  const ref = (label) => {
    const m = new RegExp(`\\[(\\d+)\\] [^\\n]*"${label}`).exec(h.last);
    assert.ok(m, `no ${label} in:\n${h.last}`);
    return Number(m[1]);
  };
  await h.call('pip', 'type', { ref: ref('Username'), value: 'pip' });
  r = await h.call('pip', 'type', { ref: ref('Password'), value: 'hunter2' });
  assert.ok(!r.text.includes('hunter2'), 'a password is never read back');
  r = await h.call('pip', 'click', { ref: ref('Sign in') });
  assert.match(r.text, /Welcome back, pip/);
  assert.ok(!h.site.hits.some((x) => x.path === '/direct'), 'a direct request to 127.0.0.1 never reached the server');
  assert.ok(!h.site.hits.some((x) => String(x.host).startsWith('tracker.test')), 'the tracker was never reached');

  // A download, then uploading it (and nothing else).
  r = await h.call('pip', 'click', { ref: ref('Get receipt') });
  assert.match(r.text, /Downloaded "receipt\.txt"/);
  r = await h.call('pip', 'type', { ref: ref('Attach'), file: '/etc/passwd' });
  assert.equal(r.isError, true);
  assert.match(r.text, /Only files in your downloads/);
  r = await h.call('pip', 'type', { ref: ref('Attach'), file: 'receipt.txt' });
  assert.ok(!r.isError, r.text);
  r = await h.call('pip', 'click', { ref: ref('Upload') });
  assert.match(r.text, /Uploaded receipt\.txt/);
  assert.deepEqual(h.site.uploads, ['receipt.txt']);

  // Over the per-file cap: not saved.
  r = await h.call('pip', 'open', { url: base + '/account' });
  r = await h.call('pip', 'click', { ref: ref('Get big file') });
  assert.match(r.text, /big\.bin" not saved/);

  // select
  r = await h.call('pip', 'select', { ref: ref('Small'), option: 'Large' });
  assert.ok(!r.isError, r.text);

  // Off the allowlist: a link, a redirect, a typed address.
  r = await h.call('pip', 'click', { ref: ref('Leave') });
  assert.equal(r.isError, true, r.text);
  assert.match(r.text, /evil\.test isn't on pip's browser allowlist/);
  assert.match(r.text, /can allow it/);
  r = await h.call('pip', 'open', { url: base + '/go-evil' });
  assert.equal(r.isError, true, r.text);
  assert.match(r.text, /evil\.test isn't on pip's browser allowlist/);
  assert.ok(!h.site.hits.some((x) => String(x.host).startsWith('evil.test')), 'evil.test was never reached');

  // Thicket, the Docker host, cloud metadata and the LAN: refused even
  // when on the allowlist.
  for (const [url, what] of [
    ['http://thicket:8080/', /thicket points at a private or local address/],
    ['http://thicket/', /thicket points at a private or local address \(172\.18\.0\.2\)/],
    ['http://host.docker.internal/', /host\.docker\.internal points at a private or local address/],
    ['http://169.254.169.254/latest/meta-data/', /169\.254\.169\.254 points at a private or local address/],
    ['http://nas.lan/', /nas\.lan points at a private or local address \(10\.0\.0\.1\)/],
    ['https://nas.lan/', /nas\.lan points at a private or local address/],
    [`http://127.0.0.1:${h.port}/`, /127\.0\.0\.1 isn't on pip's browser allowlist/],
  ]) {
    r = await h.call('pip', 'open', { url });
    assert.equal(r.isError, true, url);
    assert.match(r.text, what, url);
  }
  assert.ok(!h.site.hits.some((x) => String(x.host).startsWith('127.0.0.1')), 'loopback by address was never reached');

  // A payment form raises a floor.
  r = await h.call('pip', 'open', { url: base + '/pay' });
  assert.equal(r._meta['thicket/floor'].min_level, 'ask');

  // Screenshot
  r = await h.call('pip', 'screenshot');
  assert.equal(r.content[1].type, 'image');
  assert.ok(Buffer.from(r.content[1].data, 'base64').subarray(1, 4).toString() === 'PNG');

  // done: saved sealed; the session cookie isn't readable at rest.
  r = await h.call('pip', 'done');
  assert.match(r.text, /saved/);
  const sealed = await fsp.readFile(path.join(h.dir, 'profiles', 'pip.tbp'));
  assert.ok(!sealed.includes('s3cr3t-session'));
  assert.deepEqual(await fsp.readdir(path.join(h.dir, 'run')), [], 'no plain profile left');

  // Restart the plugin: the login is still there.
  await h.stop();
  await h.start();
  r = await h.call('pip', 'open', { url: base + '/account' });
  assert.match(r.text, /Welcome back, pip/, r.text);
  // receipt.txt came back with the profile.
  r = await h.call('pip', 'type', { ref: ref('Attach'), file: 'receipt.txt' });
  assert.ok(!r.isError, r.text);
  await h.call('pip', 'done');

  // Another sprig doesn't share it.
  r = await h.call('juni', 'open', { url: base + '/account' });
  assert.match(r.text, /Please sign in/);
  await h.call('juni', 'done');

  // A wrong key doesn't open pip's profile, and doesn't destroy it.
  const keep = h.keys.pip;
  h.keys.pip = crypto.randomBytes(32).toString('base64');
  r = await h.call('pip', 'open', { url: base + '/account' });
  assert.equal(r.isError, true);
  assert.match(r.text, /couldn't be unlocked/);
  h.keys.pip = keep;
  r = await h.call('pip', 'open', { url: base + '/account' });
  assert.match(r.text, /Welcome back, pip/);
});

test('at most max browsers, and an idle one closes and is saved', { timeout: 60000 }, async (t) => {
  const h = await harness(t, { max: 1, idleMs: 1500 });
  const base = `http://login.test:${h.port}`;
  let r = await h.call('pip', 'open', { url: base + '/' });
  assert.ok(!r.isError, r.text);
  r = await h.call('juni', 'open', { url: base + '/' });
  assert.equal(r.isError, true);
  assert.match(r.text, /browser is busy: 1 browser is open already \(pip\)/);
  r = await h.call('juni', 'read_page');
  assert.match(r.text, /No page is open/);
  await new Promise((res) => setTimeout(res, 2500));
  assert.equal(h.sessions.isOpen('pip'), false);
  assert.ok(fs.existsSync(path.join(h.dir, 'profiles', 'pip.tbp')));
  r = await h.call('juni', 'open', { url: base + '/' });
  assert.ok(!r.isError, r.text);

  // Chromium going away on its own ends the session and seals the profile.
  await h.sessions.open.get('juni').context.close();
  for (let i = 0; i < 50 && h.sessions.isOpen('juni'); i++) await new Promise((res) => setTimeout(res, 100));
  assert.equal(h.sessions.isOpen('juni'), false);
  assert.ok(fs.existsSync(path.join(h.dir, 'profiles', 'juni.tbp')));
  assert.deepEqual(await fsp.readdir(path.join(h.dir, 'run')), []);
});

test('the doctor check and the profile admin API', { timeout: 60000 }, async (t) => {
  const h = await harness(t);
  const get = (p, init = {}) => fetch(h.url + p, { ...init, headers: { authorization: 'Bearer ' + TOKEN, ...(init.headers || {}) } });
  assert.equal((await fetch(h.url + '/health')).status, 200);
  assert.equal((await fetch(h.url + '/v1/doctor')).status, 401);
  const doc = await (await get('/v1/doctor')).json();
  for (const c of doc.checks) assert.ok(c.ok, JSON.stringify(c));
  assert.deepEqual(doc.checks.map((c) => c.name), ['chromium', 'proxy_blocks_unlisted', 'proxy_blocks_private', 'proxy_blocks_private']);

  await h.call('pip', 'open', { url: `http://login.test:${h.port}/` });
  assert.equal((await get('/v1/profiles/pip', { method: 'PUT', body: 'TBP1' + 'x'.repeat(40) })).status, 409);
  await h.call('pip', 'done');
  const list = await (await get('/v1/profiles')).json();
  assert.deepEqual(list.profiles.map((p) => p.sprig), ['pip']);
  const blob = Buffer.from(await (await get('/v1/profiles/pip')).arrayBuffer());
  assert.equal(blob.subarray(0, 4).toString(), 'TBP1');
  assert.equal((await get('/v1/profiles/pip', { method: 'PUT', body: 'not sealed' })).status, 400);
  assert.equal((await get('/v1/profiles/pip', { method: 'DELETE' })).status, 200);
  assert.equal((await get('/v1/profiles/pip')).status, 404);
  assert.equal((await get('/v1/profiles/pip', { method: 'PUT', body: blob })).status, 200);
  assert.equal((await get('/v1/profiles/pip')).status, 200);
  assert.equal((await get('/v1/profiles/..%2Fetc', { method: 'GET' })).status, 400);
});
