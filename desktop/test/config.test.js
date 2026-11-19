const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { load, save, normalizeServer, sameOrigin } = require('../lib/config');

test('normalizeServer', () => {
  assert.equal(normalizeServer('thicket.example.com'), 'https://thicket.example.com');
  assert.equal(normalizeServer(' https://thicket.example.com/dashboard '), 'https://thicket.example.com');
  assert.equal(normalizeServer('localhost:8080'), 'http://localhost:8080');
  assert.equal(normalizeServer('http://192.168.1.20:8080/'), 'http://192.168.1.20:8080');
  assert.throws(() => normalizeServer(''), /Enter/);
  assert.throws(() => normalizeServer('ftp://x.example'), /https/);
  assert.throws(() => normalizeServer('https://me:pw@x.example'), /password/);
});

test('sameOrigin', () => {
  assert.ok(sameOrigin('https://t.example/c/pip', 'https://t.example'));
  assert.ok(!sameOrigin('https://t.example.evil.com/', 'https://t.example'));
  assert.ok(!sameOrigin('http://t.example/', 'https://t.example'));
  assert.ok(!sameOrigin('not a url', 'https://t.example'));
});

test('config round trip', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'thicket-desktop-'));
  assert.deepEqual(load(dir), { server: '' });
  save(dir, { server: 'https://t.example' });
  assert.deepEqual(load(dir), { server: 'https://t.example' });
  fs.writeFileSync(path.join(dir, 'config.json'), '{broken');
  assert.deepEqual(load(dir), { server: '' });
});
