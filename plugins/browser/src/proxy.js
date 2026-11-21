'use strict';
// The egress proxy: one per sprig's browser, on 127.0.0.1, and the only
// way that Chromium reaches the network (it is started with this proxy,
// no loopback bypass and a host resolver that resolves nothing, so it
// can't go around it). Every request goes through here: navigations,
// redirects (each is a new request), subresources, downloads, fetches
// and WebSockets.
//
// For each request:
//   1. the host must be on the sprig's allowlist (by name, before DNS);
//   2. the port must be 80 or 443;
//   3. the name is resolved once, and every address it resolves to must
//      be public (no loopback, LAN, link-local or cloud metadata, ...);
//   4. the connection is made to the address that was checked, never by
//      name again, so a DNS answer that changes in between (rebinding)
//      can't move it somewhere else.
// A refusal is a 403 (CONNECT or plain HTTP) and an onBlock event, which
// the tools turn into "blocked: <host> isn't on the allowlist".

const http = require('node:http');
const net = require('node:net');
const dns = require('node:dns');
const { isPublicIP, allowed, normAllow, normHost } = require('./netcheck');

class Blocked extends Error {
  constructor(host, port, kind, detail) {
    super(detail);
    this.host = host;
    this.port = port;
    this.kind = kind; // allowlist | private | port | dns | scheme
  }
}

const hopByHop = ['connection', 'proxy-connection', 'keep-alive', 'proxy-authorization', 'proxy-authenticate', 'te', 'trailer', 'transfer-encoding', 'upgrade'];

function defaultLookup(host) {
  return new Promise((resolve, reject) => {
    dns.lookup(host, { all: true, verbatim: true }, (err, addrs) => (err ? reject(err) : resolve(addrs.map((a) => a.address))));
  });
}

class EgressProxy {
  // opts.allow: the allowlist. opts.lookup(host) → Promise<[ip]> (tests
  // replace DNS). opts.unsafeLoopbackPorts: TESTS ONLY, loopback
  // addresses on these ports are allowed (never set in production: the
  // server has no setting for it). opts.onBlock(event).
  constructor(opts = {}) {
    this.allow = normAllow(opts.allow || []);
    this.lookup = opts.lookup || defaultLookup;
    this.loopbackPorts = new Set((opts.unsafeLoopbackPorts || []).map(Number));
    this.ports = new Set([80, 443]);
    this.onBlock = opts.onBlock || (() => {});
    this.connectTimeout = opts.connectTimeout || 15000;
    this.dial = opts.dial || ((ip, port) => net.connect({ host: ip, port }));
    this.sockets = new Set();
    this.server = http.createServer((req, res) => this.handleHTTP(req, res));
    this.server.on('connect', (req, sock, head) => this.handleConnect(req, sock, head));
    this.server.on('connection', (s) => {
      this.sockets.add(s);
      s.on('close', () => this.sockets.delete(s));
    });
    // Plain-HTTP upgrades through the proxy aren't used by Chromium (it
    // tunnels WebSockets with CONNECT): refuse them.
    this.server.on('upgrade', (req, sock) => sock.end('HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n'));
  }

  setAllow(list) {
    this.allow = normAllow(list);
  }

  listen() {
    return new Promise((resolve, reject) => {
      this.server.once('error', reject);
      this.server.listen(0, '127.0.0.1', () => {
        this.port = this.server.address().port;
        resolve(this.port);
      });
    });
  }

  close() {
    for (const s of this.sockets) s.destroy();
    return new Promise((resolve) => this.server.close(() => resolve()));
  }

  // check decides one destination and returns the address to dial, or
  // throws Blocked.
  async check(rawHost, port) {
    const host = normHost(rawHost);
    port = Number(port);
    if (!host) throw new Blocked(rawHost, port, 'allowlist', 'no host');
    if (!allowed(this.allow, host)) {
      throw new Blocked(host, port, 'allowlist', `${host} isn't on the browser allowlist`);
    }
    let addrs;
    if (net.isIP(host)) {
      addrs = [host];
    } else {
      try {
        addrs = await this.lookup(host);
      } catch (e) {
        throw new Blocked(host, port, 'dns', `${host} couldn't be found (${e.code || e.message})`);
      }
    }
    if (!addrs || addrs.length === 0) throw new Blocked(host, port, 'dns', `${host} couldn't be found`);
    // Every address must be public: a name that also points at the LAN
    // is refused outright, not tried on its good address. (Tests may
    // allow loopback on their own ports; production never sets that.)
    const testLoopback = this.loopbackPorts.has(port) && addrs.every((ip) => ip === '127.0.0.1' || ip === '::1');
    if (!testLoopback) {
      for (const ip of addrs) {
        if (!isPublicIP(ip)) {
          throw new Blocked(host, port, 'private', `${host} points at a private or local address (${ip}), which the browser may never reach`);
        }
      }
    }
    if (!this.ports.has(port) && !testLoopback) {
      throw new Blocked(host, port, 'port', `${host}:${port} is refused: the browser only uses ports 80 and 443`);
    }
    return addrs[0];
  }

  blocked(err) {
    const ev = { host: err.host, port: err.port, kind: err.kind, detail: err.message, at: Date.now() };
    try {
      this.onBlock(ev);
    } catch {
      // the listener's problem
    }
    return ev;
  }

  async handleConnect(req, client, head) {
    client.on('error', () => {});
    let host, port;
    try {
      const u = new URL('http://' + req.url);
      host = u.hostname;
      port = Number(u.port || 443);
    } catch {
      client.end('HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n');
      return;
    }
    let ip;
    try {
      ip = await this.check(host, port);
    } catch (e) {
      if (!(e instanceof Blocked)) throw e;
      const ev = this.blocked(e);
      client.end(`HTTP/1.1 403 Forbidden\r\nX-Thicket-Blocked: ${ev.kind}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
      return;
    }
    const upstream = this.dial(ip, port);
    const timer = setTimeout(() => upstream.destroy(new Error('connect timeout')), this.connectTimeout);
    upstream.once('connect', () => {
      clearTimeout(timer);
      client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head && head.length) upstream.write(head);
      upstream.pipe(client);
      client.pipe(upstream);
    });
    upstream.on('error', () => {
      clearTimeout(timer);
      if (!client.destroyed) client.end('HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n');
    });
    client.on('close', () => upstream.destroy());
    upstream.on('close', () => client.destroy());
  }

  async handleHTTP(req, res) {
    let u;
    try {
      u = new URL(req.url);
    } catch {
      res.writeHead(400).end();
      return;
    }
    if (u.protocol !== 'http:') {
      const ev = this.blocked(new Blocked(u.hostname, 0, 'scheme', `${u.protocol} isn't allowed`));
      this.refuse(res, ev);
      return;
    }
    const port = Number(u.port || 80);
    let ip;
    try {
      ip = await this.check(u.hostname, port);
    } catch (e) {
      if (!(e instanceof Blocked)) throw e;
      this.refuse(res, this.blocked(e));
      return;
    }
    const headers = {};
    for (const [k, v] of Object.entries(req.headers)) {
      if (!hopByHop.includes(k.toLowerCase())) headers[k] = v;
    }
    headers.host = u.host;
    const up = http.request({
      host: ip, port, method: req.method, path: u.pathname + u.search, headers, setHost: false,
      createConnection: () => this.dial(ip, port), timeout: 60000,
    }, (ures) => {
      const h = {};
      for (const [k, v] of Object.entries(ures.headers)) {
        if (!hopByHop.includes(k.toLowerCase())) h[k] = v;
      }
      res.writeHead(ures.statusCode || 502, ures.statusMessage, h);
      ures.pipe(res);
    });
    up.on('timeout', () => up.destroy(new Error('timeout')));
    up.on('error', () => {
      if (!res.headersSent) res.writeHead(502, { 'content-type': 'text/plain' });
      res.end();
    });
    req.pipe(up);
  }

  refuse(res, ev) {
    const body = `<!doctype html><title>Blocked by Thicket</title><h1>Blocked</h1><p>${escapeHTML(ev.detail)}.</p>`;
    res.writeHead(403, { 'content-type': 'text/html; charset=utf-8', 'x-thicket-blocked': ev.kind, 'cache-control': 'no-store', 'content-length': Buffer.byteLength(body) });
    res.end(body);
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

module.exports = { EgressProxy, Blocked };
