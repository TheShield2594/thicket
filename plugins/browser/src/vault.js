'use strict';
// A sprig's browser profile at rest: one encrypted file per sprig.
//
// While a sprig's browser is open its profile (cookies, local storage,
// downloads) is a plain folder on a tmpfs (memory, never the disk).
// When the browser closes, the folder is packed into a simple archive,
// gzipped and sealed with AES-256-GCM under the sprig's profile key,
// which Thicket keeps in its encrypted secret store and sends with each
// call; the plain folder is then deleted. Opening reverses it. The plugin
// never writes the key anywhere.
//
// File: "TBP1" | 12-byte random nonce | ciphertext | 16-byte GCM tag.
// The additional authenticated data names the sprig, so one sprig's file
// copied over another's doesn't open. A wrong key or a changed byte
// fails to open, and the file is left as it was.
//
// Archive (inside the gzip): repeated records
//   type u8 (1 folder, 2 file) | mode u16 | path length u16 | path (UTF-8)
//   | [file only] size u64 | bytes
// Paths are relative, '/'-separated, never absolute or with '..'.
// Caches Chromium can rebuild are left out (see skip).

const crypto = require('node:crypto');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const path = require('node:path');
const zlib = require('node:zlib');

const MAGIC = Buffer.from('TBP1');

// Chromium caches and lock files: rebuilt on start, so not kept.
const skipNames = new Set([
  'Cache', 'Code Cache', 'GPUCache', 'GrShaderCache', 'ShaderCache', 'GraphiteDawnCache', 'DawnCache',
  'DawnGraphiteCache', 'DawnWebGPUCache', 'component_crx_cache', 'extensions_crx_cache', 'CacheStorage',
  'ScriptCache', 'Crashpad', 'BrowserMetrics', 'optimization_guide_model_store', 'Safe Browsing',
  'SingletonLock', 'SingletonCookie', 'SingletonSocket', 'RunningChromeVersion', '.dl-tmp',
]);

function aad(sprig) {
  return Buffer.from('thicket-browser-profile:v1:' + sprig);
}

function keyFrom(b64) {
  const key = Buffer.from(String(b64 || ''), 'base64');
  if (key.length !== 32) throw new Error('the profile key must be 32 bytes, base64');
  return key;
}

function seal(plain, key, sprig) {
  const nonce = crypto.randomBytes(12);
  const c = crypto.createCipheriv('aes-256-gcm', key, nonce);
  c.setAAD(aad(sprig));
  const body = Buffer.concat([c.update(plain), c.final()]);
  return Buffer.concat([MAGIC, nonce, body, c.getAuthTag()]);
}

function open(sealed, key, sprig) {
  if (sealed.length < 4 + 12 + 16 || !sealed.subarray(0, 4).equals(MAGIC)) throw new Error('not a browser profile file');
  const nonce = sealed.subarray(4, 16);
  const tag = sealed.subarray(sealed.length - 16);
  const d = crypto.createDecipheriv('aes-256-gcm', key, nonce);
  d.setAAD(aad(sprig));
  d.setAuthTag(tag);
  try {
    return Buffer.concat([d.update(sealed.subarray(16, sealed.length - 16)), d.final()]);
  } catch {
    throw new Error("the saved profile couldn't be unlocked with the key Thicket sent (a different key, or the file was changed)");
  }
}

// pack walks dir and returns the archive, refusing more than max bytes.
async function pack(dir, max) {
  const parts = [];
  let total = 0;
  const add = (b) => {
    total += b.length;
    if (total > max) throw new Error(`the profile is bigger than ${Math.round(max / 1048576)} MB`);
    parts.push(b);
  };
  async function walk(rel) {
    const entries = await fsp.readdir(path.join(dir, rel), { withFileTypes: true });
    entries.sort((a, b) => (a.name < b.name ? -1 : 1));
    for (const e of entries) {
      if (skipNames.has(e.name)) continue;
      const r = rel ? rel + '/' + e.name : e.name;
      const full = path.join(dir, r);
      if (e.isDirectory()) {
        add(header(1, 0o700, r));
        await walk(r);
      } else if (e.isFile()) {
        const data = await fsp.readFile(full);
        const h = header(2, 0o600, r);
        const size = Buffer.alloc(8);
        size.writeBigUInt64BE(BigInt(data.length));
        add(h);
        add(size);
        add(data);
      } // symlinks, sockets and the like are never kept
    }
  }
  await walk('');
  return Buffer.concat(parts);
}

function header(type, mode, rel) {
  const p = Buffer.from(rel, 'utf8');
  if (p.length > 65535) throw new Error('path too long');
  const h = Buffer.alloc(5);
  h.writeUInt8(type, 0);
  h.writeUInt16BE(mode, 1);
  h.writeUInt16BE(p.length, 3);
  return Buffer.concat([h, p]);
}

function safeRel(rel) {
  if (!rel || rel.includes('\0') || rel.includes('\\') || rel.startsWith('/')) return false;
  return rel.split('/').every((s) => s !== '' && s !== '.' && s !== '..');
}

// unpack writes an archive into dir (which must be empty or missing).
async function unpack(buf, dir) {
  await fsp.mkdir(dir, { recursive: true, mode: 0o700 });
  let off = 0;
  while (off < buf.length) {
    if (off + 5 > buf.length) throw new Error('profile archive is truncated');
    const type = buf.readUInt8(off);
    const plen = buf.readUInt16BE(off + 3);
    off += 5;
    const rel = buf.subarray(off, off + plen).toString('utf8');
    off += plen;
    if (!safeRel(rel)) throw new Error('profile archive has an unsafe path');
    const full = path.join(dir, ...rel.split('/'));
    if (type === 1) {
      await fsp.mkdir(full, { recursive: true, mode: 0o700 });
    } else if (type === 2) {
      if (off + 8 > buf.length) throw new Error('profile archive is truncated');
      const size = Number(buf.readBigUInt64BE(off));
      off += 8;
      if (off + size > buf.length) throw new Error('profile archive is truncated');
      await fsp.mkdir(path.dirname(full), { recursive: true, mode: 0o700 });
      await fsp.writeFile(full, buf.subarray(off, off + size), { mode: 0o600 });
      off += size;
    } else {
      throw new Error('profile archive is damaged');
    }
  }
}

// save seals dir into file, atomically (a crash leaves the old file).
async function save(dir, file, keyB64, sprig, max) {
  const plain = zlib.gzipSync(await pack(dir, max), { level: 6 });
  const sealed = seal(plain, keyFrom(keyB64), sprig);
  const tmp = file + '.tmp-' + process.pid;
  const fh = await fsp.open(tmp, 'w', 0o600);
  try {
    await fh.writeFile(sealed);
    await fh.sync();
  } finally {
    await fh.close();
  }
  await fsp.rename(tmp, file);
  return sealed.length;
}

// load opens file into dir. A missing file is a fresh profile (false).
async function load(file, dir, keyB64, sprig, max) {
  let sealed;
  try {
    sealed = await fsp.readFile(file);
  } catch (e) {
    if (e.code === 'ENOENT') {
      await fsp.mkdir(dir, { recursive: true, mode: 0o700 });
      return false;
    }
    throw e;
  }
  const plain = zlib.gunzipSync(open(sealed, keyFrom(keyB64), sprig), { maxOutputLength: max });
  await unpack(plain, dir);
  return true;
}

// looksSealed checks a file someone hands in (a restored backup).
function looksSealed(buf) {
  return Buffer.isBuffer(buf) && buf.length >= 32 && buf.subarray(0, 4).equals(MAGIC);
}

function existsSync(file) {
  return fs.existsSync(file);
}

module.exports = { save, load, seal, open, pack, unpack, keyFrom, looksSealed, existsSync };
