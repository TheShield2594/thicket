'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const net = require('node:net');
const { EgressProxy } = require('../src/proxy');

// A fake DNS: names Thicket's compose network would resolve, plus a site
// on this machine for the tests.
function fakeDNS(extra = {}) {
  const table = {
    'site.test': ['127.0.0.1'],
    thicket: ['172.18.0.2'],
    'host.docker.internal': ['172.17.0.1'],
    'router.lan': ['192.168.1.1'],
    'nas.lan': ['10.0.0.5'],
    'metadata.google.internal': ['169.254.169.254'],
    'v6local.test': ['::1'],
    'mixed.test': ['93.184.216.34', '10.0.0.7'],
    'public.test': ['93.184.216.34'],
    ...extra,
  };
  const calls = [];
  const lookup = async (h) => {
    calls.push(h);
    const v = table[h];
    if (typeof v === 'function') return v();
    if (!v) throw Object.assign(new Error('not found'), { code: 'ENOTFOUND' });
    return v;
  };
  return { lookup, calls };
}

function listen(server) {
  return new Promise((r) => server.listen(0, '127.0.0.1', () => r(server.address().port)));
}

function viaProxy(proxyPort, url) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port: proxyPort, method: 'GET', path: url, headers: { host: new URL(url).host } }, (res) => {
      let body = '';
      res.on('data', (c) => (body += c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body }));
    });
    req.on('error', reject);
    req.end();
  });
}

function connectVia(proxyPort, target) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port: proxyPort, method: 'CONNECT', path: target });
    req.on('connect', (res, socket) => resolve({ status: res.statusCode, headers: res.headers, socket }));
    req.on('error', reject);
    req.end();
  });
}

test('the proxy refuses hosts off the allowlist and private addresses on it, before dialling', async (t) => {
  const dns = fakeDNS();
  const dialed = [];
  const blocks = [];
  const p = new EgressProxy({
    allow: ['thicket', 'host.docker.internal', '169.254.169.254', 'router.lan', 'nas.lan', 'metadata.google.internal', 'v6local.test', 'mixed.test', 'localhost', '127.0.0.1', 'public.test'],
    lookup: dns.lookup,
    dial: (ip, port) => {
      dialed.push([ip, port]);
      return net.connect({ host: '127.0.0.1', port: 9 });
    },
    onBlock: (e) => blocks.push(e),
  });
  const port = await p.listen();
  t.after(() => p.close());

  for (const [url, kind] of [
    ['http://thicket:8080/', 'private'],
    ['http://thicket/', 'private'],
    ['http://host.docker.internal/', 'private'],
    ['http://169.254.169.254/latest/meta-data/', 'private'],
    ['http://metadata.google.internal/', 'private'],
    ['http://router.lan/', 'private'],
    ['http://nas.lan/', 'private'],
    ['http://[::1]/', 'allowlist'],
    ['http://v6local.test/', 'private'],
    ['http://mixed.test/', 'private'],
    ['http://127.0.0.1/', 'private'],
    ['http://evil.test/', 'allowlist'],
    ['http://public.test:22/', 'port'],
  ]) {
    const r = await viaProxy(port, url);
    assert.equal(r.status, 403, url);
    assert.equal(r.headers['x-thicket-blocked'], kind, url);
  }
  // HTTPS goes by CONNECT: refused the same way.
  for (const target of ['thicket:443', '169.254.169.254:443', 'evil.test:443', '10.1.2.3:443']) {
    const r = await connectVia(port, target);
    assert.equal(r.status, 403, target);
    r.socket.destroy();
  }
  assert.deepEqual(dialed, [], 'nothing blocked was ever dialled');
  assert.ok(!dns.calls.includes('evil.test'), 'a host off the list is not even resolved');
  assert.ok(blocks.some((b) => b.host === 'evil.test' && b.kind === 'allowlist'));
});

test('the proxy dials the address it checked (no DNS rebinding)', async (t) => {
  let n = 0;
  const dns = fakeDNS({ 'rebind.test': () => (n++ === 0 ? ['93.184.216.34'] : ['127.0.0.1']) });
  const dialed = [];
  const p = new EgressProxy({
    allow: ['rebind.test'],
    lookup: dns.lookup,
    dial: (ip, port) => {
      dialed.push(ip);
      const s = new net.Socket();
      process.nextTick(() => s.destroy(new Error('no network in tests')));
      return s;
    },
  });
  const port = await p.listen();
  t.after(() => p.close());
  const r = await viaProxy(port, 'http://rebind.test/');
  assert.equal(r.status, 502);
  assert.deepEqual(dialed, ['93.184.216.34']);
  assert.equal(dns.calls.filter((h) => h === 'rebind.test').length, 1, 'resolved once per request');
});

test('allowed requests pass, plain HTTP and CONNECT (test loopback seam)', async (t) => {
  const site = http.createServer((req, res) => res.end(`hello ${req.headers.host} ${req.url}`));
  const sitePort = await listen(site);
  t.after(() => site.close());
  const echo = net.createServer((s) => s.pipe(s));
  const echoPort = await listen(echo);
  t.after(() => echo.close());

  const p = new EgressProxy({ allow: ['site.test'], lookup: fakeDNS().lookup, unsafeLoopbackPorts: [sitePort, echoPort] });
  const port = await p.listen();
  t.after(() => p.close());
  const r = await viaProxy(port, `http://site.test:${sitePort}/x?y=1`);
  assert.equal(r.status, 200);
  assert.equal(r.body, `hello site.test:${sitePort} /x?y=1`);

  const c = await connectVia(port, `site.test:${echoPort}`);
  assert.equal(c.status, 200);
  const got = await new Promise((resolve) => {
    c.socket.once('data', (d) => resolve(String(d)));
    c.socket.write('ping');
  });
  assert.equal(got, 'ping');
  c.socket.destroy();

  // The seam allows loopback only: not a private address on that port,
  // and not loopback on another port.
  const p2 = new EgressProxy({ allow: ['thicket', 'site.test'], lookup: fakeDNS().lookup, unsafeLoopbackPorts: [sitePort] });
  const port2 = await p2.listen();
  t.after(() => p2.close());
  assert.equal((await viaProxy(port2, `http://thicket:${sitePort}/`)).status, 403);
  assert.equal((await viaProxy(port2, `http://site.test:${echoPort}/`)).status, 403);

  // A removal takes effect on the next request.
  p.setAllow([]);
  assert.equal((await viaProxy(port, `http://site.test:${sitePort}/`)).status, 403);
});
