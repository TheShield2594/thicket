// Settings for the desktop app: which Thicket server to open, kept in a
// small JSON file in the app's user-data folder.
const fs = require('node:fs');
const path = require('node:path');

function load(dir) {
  try {
    const c = JSON.parse(fs.readFileSync(path.join(dir, 'config.json'), 'utf8'));
    return { server: typeof c.server === 'string' ? c.server : '' };
  } catch {
    return { server: '' };
  }
}

function save(dir, config) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify(config, null, 2));
}

// normalizeServer turns what someone typed into an origin, or throws a
// message for them. Plain http is fine on this machine or a home network;
// anything else should be https.
function normalizeServer(input) {
  let s = String(input || '').trim();
  if (!s) throw new Error('Enter your Thicket server’s address.');
  if (!/^[a-z]+:\/\//i.test(s)) s = (/^(localhost|127\.|\[::1\])/.test(s) ? 'http://' : 'https://') + s;
  let u;
  try { u = new URL(s); } catch { throw new Error('That doesn’t look like an address. Try something like thicket.example.com or http://192.168.1.20:8080'); }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new Error('The address must start with https:// or http://');
  if (u.username || u.password) throw new Error('Leave the username and password out of the address.');
  return u.origin;
}

// sameOrigin reports whether url belongs to the configured server.
function sameOrigin(url, server) {
  try { return new URL(url).origin === server; } catch { return false; }
}

module.exports = { load, save, normalizeServer, sameOrigin };
