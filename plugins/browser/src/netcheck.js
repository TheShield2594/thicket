'use strict';
// What the egress proxy may reach: the sprig's allowlist (by host name),
// and only public addresses (checked after DNS, on the address that is
// then dialled).

const net = require('node:net');

// Never reachable, whatever the allowlist says: this machine, the LAN,
// link-local (cloud metadata at 169.254.169.254), carrier-grade NAT,
// documentation and benchmarking ranges, multicast and reserved space.
const blockedV4 = [
  ['0.0.0.0', 8], ['10.0.0.0', 8], ['100.64.0.0', 10], ['127.0.0.0', 8],
  ['169.254.0.0', 16], ['172.16.0.0', 12], ['192.0.0.0', 24], ['192.0.2.0', 24],
  ['192.88.99.0', 24], ['192.168.0.0', 16], ['198.18.0.0', 15], ['198.51.100.0', 24],
  ['203.0.113.0', 24], ['224.0.0.0', 4], ['240.0.0.0', 4],
];
// IPv6: only global unicast (2000::/3) is considered at all, minus these.
const blockedV6 = [
  ['64:ff9b::', 96], ['64:ff9b:1::', 48], ['100::', 64], ['2001::', 23],
  ['2001:db8::', 32], ['2002::', 16], ['3fff::', 20],
];

const v4 = new net.BlockList();
for (const [a, p] of blockedV4) v4.addSubnet(a, p, 'ipv4');
const v6 = new net.BlockList();
for (const [a, p] of blockedV6) v6.addSubnet(a, p, 'ipv6');
const global6 = new net.BlockList();
global6.addSubnet('2000::', 3, 'ipv6');

// stripBrackets turns "[::1]" into "::1".
function stripBrackets(host) {
  return host.startsWith('[') && host.endsWith(']') ? host.slice(1, -1) : host;
}

// isPublicIP reports whether ip is an address the browser may reach.
function isPublicIP(ip) {
  ip = stripBrackets(String(ip)).split('%')[0];
  const fam = net.isIP(ip);
  if (fam === 4) return !v4.check(ip, 'ipv4');
  if (fam !== 6) return false;
  // Outside 2000::/3: ::1, ::ffff:a.b.c.d (v4 dressed up as v6), fc00::/7,
  // fe80::/10, ff00::/8 and the rest.
  if (!global6.check(ip, 'ipv6')) return false;
  return !v6.check(ip, 'ipv6');
}

// normHost lower-cases a host and drops a trailing dot and brackets.
function normHost(host) {
  return stripBrackets(String(host || '').trim().toLowerCase()).replace(/\.$/, '');
}

// normAllow cleans an allowlist from core: lower case, no "*." prefix,
// no trailing dot, empty entries dropped.
function normAllow(list) {
  const out = new Set();
  for (const e of Array.isArray(list) ? list : []) {
    if (typeof e !== 'string') continue;
    const v = normHost(e).replace(/^\*\./, '').replace(/^\./, '');
    if (v) out.add(v);
  }
  return [...out];
}

// allowed reports whether host is on the allowlist: an entry matches
// itself and its subdomains ("example.com" allows www.example.com, not
// badexample.com). An IP-address host matches only an identical entry.
function allowed(list, host) {
  host = normHost(host);
  if (!host) return false;
  const isIP = net.isIP(host) !== 0;
  for (const e of list) {
    if (host === e) return true;
    if (!isIP && host.endsWith('.' + e)) return true;
  }
  return false;
}

module.exports = { isPublicIP, allowed, normAllow, normHost };
