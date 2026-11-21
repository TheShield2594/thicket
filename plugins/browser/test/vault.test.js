'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const vault = require('../src/vault');

const key = () => crypto.randomBytes(32).toString('base64');

async function tmpdir(t) {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), 'tb-vault-'));
  t.after(() => fsp.rm(d, { recursive: true, force: true }));
  return d;
}

test('a profile round-trips sealed, and caches and locks are left out', async (t) => {
  const d = await tmpdir(t);
  const src = path.join(d, 'src');
  await fsp.mkdir(path.join(src, 'profile', 'Default', 'Cache'), { recursive: true });
  await fsp.writeFile(path.join(src, 'profile', 'Default', 'Cookies'), 'session=SECRET-COOKIE-VALUE');
  await fsp.writeFile(path.join(src, 'profile', 'Default', 'Cache', 'big'), 'x'.repeat(1000));
  await fsp.mkdir(path.join(src, 'downloads'));
  await fsp.writeFile(path.join(src, 'downloads', 'receipt.pdf'), Buffer.from([1, 2, 3, 0, 255]));
  fs.symlinkSync('/etc/passwd', path.join(src, 'profile', 'SingletonLock'));
  fs.symlinkSync('/etc/passwd', path.join(src, 'profile', 'link'));
  const k = key();
  const file = path.join(d, 'pip.tbp');
  await vault.save(src, file, k, 'pip', 1 << 20);
  const raw = await fsp.readFile(file);
  assert.ok(vault.looksSealed(raw));
  assert.ok(!raw.includes('SECRET-COOKIE-VALUE'), 'not readable at rest');

  const out = path.join(d, 'out');
  assert.equal(await vault.load(file, out, k, 'pip', 1 << 20), true);
  assert.equal(await fsp.readFile(path.join(out, 'profile', 'Default', 'Cookies'), 'utf8'), 'session=SECRET-COOKIE-VALUE');
  assert.deepEqual([...(await fsp.readFile(path.join(out, 'downloads', 'receipt.pdf')))], [1, 2, 3, 0, 255]);
  assert.ok(!fs.existsSync(path.join(out, 'profile', 'Default', 'Cache')));
  assert.ok(!fs.existsSync(path.join(out, 'profile', 'link')), 'symlinks are never kept');
});

test('a wrong key, another sprig or a changed byte does not open, and the file is kept', async (t) => {
  const d = await tmpdir(t);
  const src = path.join(d, 'src');
  await fsp.mkdir(src);
  await fsp.writeFile(path.join(src, 'a'), 'hello');
  const k = key();
  const file = path.join(d, 'pip.tbp');
  await vault.save(src, file, k, 'pip', 1 << 20);
  const before = await fsp.readFile(file);
  await assert.rejects(vault.load(file, path.join(d, 'o1'), key(), 'pip', 1 << 20), /couldn't be unlocked/);
  await assert.rejects(vault.load(file, path.join(d, 'o2'), k, 'juni', 1 << 20), /couldn't be unlocked/);
  const bad = Buffer.from(before);
  bad[30] ^= 1;
  await fsp.writeFile(path.join(d, 'bad.tbp'), bad);
  await assert.rejects(vault.load(path.join(d, 'bad.tbp'), path.join(d, 'o3'), k, 'pip', 1 << 20), /couldn't be unlocked/);
  assert.deepEqual(await fsp.readFile(file), before);
  await assert.rejects(vault.load(file, path.join(d, 'o4'), 'c2hvcnQ=', 'pip', 1 << 20), /32 bytes/);
  // No file: a fresh profile.
  assert.equal(await vault.load(path.join(d, 'none.tbp'), path.join(d, 'o5'), k, 'pip', 1 << 20), false);
});

test('an archive with an unsafe path is refused', async (t) => {
  const d = await tmpdir(t);
  const rel = Buffer.from('../escape');
  const h = Buffer.alloc(5);
  h.writeUInt8(2, 0);
  h.writeUInt16BE(0o600, 1);
  h.writeUInt16BE(rel.length, 3);
  const size = Buffer.alloc(8);
  size.writeBigUInt64BE(2n);
  await assert.rejects(vault.unpack(Buffer.concat([h, rel, size, Buffer.from('hi')]), path.join(d, 'x')), /unsafe path/);
  assert.ok(!fs.existsSync(path.join(d, 'escape')));
});

test('a profile over the size limit is refused', async (t) => {
  const d = await tmpdir(t);
  await fsp.writeFile(path.join(d, 'big'), Buffer.alloc(4096));
  await assert.rejects(vault.pack(d, 1024), /bigger than/);
});

// Chromium can still be tidying its profile folder when a session that
// ended on its own is saved: a file listed a moment ago may be gone.
test('a file or folder that vanishes while the profile is saved is left out, not fatal', async (t) => {
  const d = await tmpdir(t);
  const src = path.join(d, 'src');
  await fsp.mkdir(path.join(src, 'profile', 'Default', 'tmp'), { recursive: true });
  await fsp.writeFile(path.join(src, 'profile', 'Default', 'Cookies'), 'session=1');
  await fsp.writeFile(path.join(src, 'profile', 'Default', 'Cookies-journal'), 'j');
  await fsp.writeFile(path.join(src, 'profile', 'Default', 'tmp', 'x'), 'x');
  const gone = [path.join(src, 'profile', 'Default', 'Cookies-journal'), path.join(src, 'profile', 'Default', 'tmp')];
  // Remove each just as the save reaches it.
  const readFile = fsp.readFile, readdir = fsp.readdir;
  t.after(() => { fsp.readFile = readFile; fsp.readdir = readdir; });
  const vanish = (real) => async (p, ...rest) => {
    if (gone.includes(p)) await fsp.rm(p, { recursive: true, force: true });
    return real(p, ...rest);
  };
  fsp.readFile = vanish(readFile);
  fsp.readdir = vanish(readdir);
  const k = key();
  const file = path.join(d, 'pip.tbp');
  await vault.save(src, file, k, 'pip', 1 << 20);
  fsp.readFile = readFile; fsp.readdir = readdir;

  const out = path.join(d, 'out');
  assert.equal(await vault.load(file, out, k, 'pip', 1 << 20), true);
  assert.equal(await fsp.readFile(path.join(out, 'profile', 'Default', 'Cookies'), 'utf8'), 'session=1');
  assert.ok(!fs.existsSync(path.join(out, 'profile', 'Default', 'Cookies-journal')));
});
