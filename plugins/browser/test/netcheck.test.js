'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { isPublicIP, allowed, normAllow } = require('../src/netcheck');

test('private, loopback, link-local and metadata addresses are never public', () => {
  for (const ip of ['127.0.0.1', '127.8.9.10', '10.0.0.1', '172.16.0.1', '172.17.0.1', '172.31.255.255', '192.168.1.10',
    '169.254.169.254', '100.64.0.1', '0.0.0.0', '224.0.0.1', '255.255.255.255', '198.18.0.1', '192.0.2.5',
    '::1', '::', 'fe80::1', 'fd00::1', 'fc00::5', 'ff02::1', '::ffff:127.0.0.1', '::ffff:10.0.0.1', '::ffff:7f00:1',
    '64:ff9b::a00:1', '2001:db8::1', '2002:a00:1::1', '[::1]', 'fe80::1%eth0', 'not-an-ip', '']) {
    assert.equal(isPublicIP(ip), false, ip);
  }
  for (const ip of ['1.1.1.1', '8.8.8.8', '93.184.216.34', '172.15.0.1', '172.32.0.1', '2606:4700:4700::1111', '2a00:1450::1']) {
    assert.equal(isPublicIP(ip), true, ip);
  }
});

test('allowlist entries match the domain and its subdomains only', () => {
  const list = normAllow(['Example.COM', '*.shop.test', ' bank.test. ', '', 7]);
  assert.deepEqual(list.sort(), ['bank.test', 'example.com', 'shop.test']);
  assert.ok(allowed(list, 'example.com'));
  assert.ok(allowed(list, 'www.example.com'));
  assert.ok(allowed(list, 'WWW.EXAMPLE.COM.'));
  assert.ok(allowed(list, 'cdn.shop.test'));
  assert.ok(!allowed(list, 'badexample.com'));
  assert.ok(!allowed(list, 'example.com.evil.test'));
  assert.ok(!allowed(list, 'test'));
  assert.ok(!allowed([], 'example.com'));
  assert.ok(allowed(['1.2.3.4'], '1.2.3.4'));
  assert.ok(!allowed(['2.3.4'], '1.2.3.4'), 'an IP never matches as a subdomain');
});
