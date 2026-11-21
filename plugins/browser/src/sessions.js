'use strict';
// One Chromium per active sprig, each with its own profile, its own
// egress proxy and its own downloads folder. At most `max` are open at
// once; one left alone for `idleMs` closes (and its profile is saved,
// encrypted). Calls for one sprig run one at a time.

const fsp = require('node:fs/promises');
const path = require('node:path');
const { EgressProxy } = require('./proxy');
const vault = require('./vault');

const sprigRE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

class UserError extends Error {}

// chromiumArgs: Chromium reaches the network only through the sprig's
// proxy. <-loopback> removes Chromium's built-in "localhost goes direct"
// exception; the resolver rule makes any lookup Chromium tries itself
// fail (proxied requests are resolved by the proxy, which checks them);
// WebRTC may not use UDP outside the proxy; QUIC is off.
function chromiumArgs(proxyPort) {
  return [
    `--proxy-server=http://127.0.0.1:${proxyPort}`,
    '--proxy-bypass-list=<-loopback>',
    '--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1',
    '--disable-quic',
    '--force-webrtc-ip-handling-policy',
    '--webrtc-ip-handling-policy=disable_non_proxied_udp',
    '--disable-background-networking',
    '--disable-component-update',
    '--disable-domain-reliability',
    '--disable-sync',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-dev-shm-usage',
    '--disk-cache-size=33554432',
    '--password-store=basic',
  ];
}

class Session {
  constructor(sprig, key, dir) {
    this.sprig = sprig;
    this.key = key;
    this.dir = dir;
    this.profileDir = path.join(dir, 'profile');
    this.downloadsDir = path.join(dir, 'downloads');
    this.tmpDir = path.join(dir, '.dl-tmp');
    this.blocks = [];
    this.downloads = []; // finished this session: {name, size} or {name, error}
    this.pending = new Set();
    this.lastNavFailure = null;
    this.openedAt = Date.now();
    this.lastUsed = Date.now();
    this.timer = null;
  }
}

class Sessions {
  constructor(opts) {
    this.chromium = opts.chromium;
    this.profilesDir = opts.profilesDir;
    this.runDir = opts.runDir;
    this.max = opts.max || 2;
    this.idleMs = opts.idleMs || 10 * 60 * 1000;
    this.maxProfile = opts.maxProfileBytes || 200 * 1048576;
    this.downloadsCap = opts.downloadsCapBytes || 50 * 1048576;
    this.fileCap = opts.fileCapBytes || 25 * 1048576;
    this.proxyOpts = opts.proxy || {};
    this.headless = opts.headless !== false;
    this.log = opts.log || (() => {});
    this.open = new Map();
    this.starting = 0;
    this.locks = new Map();
  }

  async init() {
    await fsp.mkdir(this.profilesDir, { recursive: true, mode: 0o700 });
    await fsp.mkdir(this.runDir, { recursive: true, mode: 0o700 });
    // A plain profile left by a crash is not kept: the sealed copy is.
    for (const e of await fsp.readdir(this.runDir)) {
      await fsp.rm(path.join(this.runDir, e), { recursive: true, force: true });
    }
    for (const e of await fsp.readdir(this.profilesDir)) {
      if (e.includes('.tmp-')) await fsp.rm(path.join(this.profilesDir, e), { force: true });
    }
  }

  static validSprig(sprig) {
    return typeof sprig === 'string' && sprigRE.test(sprig);
  }

  profileFile(sprig) {
    return path.join(this.profilesDir, sprig + '.tbp');
  }

  isOpen(sprig) {
    return this.open.has(sprig);
  }

  list() {
    return [...this.open.values()].map((s) => ({ sprig: s.sprig, opened_at: new Date(s.openedAt).toISOString(), last_used: new Date(s.lastUsed).toISOString() }));
  }

  // lock runs fn after any earlier call for the same sprig.
  lock(sprig, fn) {
    const prev = this.locks.get(sprig) || Promise.resolve();
    const run = prev.catch(() => {}).then(fn);
    const tail = run.catch(() => {});
    this.locks.set(sprig, tail);
    tail.then(() => {
      if (this.locks.get(sprig) === tail) this.locks.delete(sprig);
    });
    return run;
  }

  // use runs fn(session) for sprig, opening its browser if start is set.
  async use(sprig, key, allow, fn, { start = true } = {}) {
    if (!Sessions.validSprig(sprig)) throw new UserError('Thicket didn\'t say which sprig is calling');
    vault.keyFrom(key);
    return this.lock(sprig, async () => {
      let s = this.open.get(sprig);
      if (s && s.key !== key) throw new UserError('the profile key for this sprig changed while its browser was open; close it with done and try again');
      if (!s) {
        if (!start) return fn(null);
        s = await this.start(sprig, key, allow);
      }
      s.proxy.setAllow(allow);
      this.touch(s);
      try {
        return await fn(s);
      } finally {
        this.touch(s);
      }
    });
  }

  touch(s) {
    s.lastUsed = Date.now();
    clearTimeout(s.timer);
    s.timer = setTimeout(() => {
      this.close(s.sprig, 'idle').catch((e) => this.log('idle close failed', s.sprig, e.message));
    }, this.idleMs);
    s.timer.unref();
  }

  async start(sprig, key, allow) {
    if (this.open.size + this.starting >= this.max) {
      const who = [...this.open.keys()].join(', ');
      throw new UserError(`the browser is busy: ${this.max} browser${this.max === 1 ? ' is' : 's are'} open already (${who || 'starting'}). One closes after 10 minutes unused, or when its sprig says done; try again then`);
    }
    this.starting++;
    const dir = path.join(this.runDir, sprig);
    const s = new Session(sprig, key, dir);
    try {
      await fsp.rm(dir, { recursive: true, force: true });
      await vault.load(this.profileFile(sprig), dir, key, sprig, this.maxProfile);
      await fsp.mkdir(s.profileDir, { recursive: true, mode: 0o700 });
      await fsp.mkdir(s.downloadsDir, { recursive: true, mode: 0o700 });
      await fsp.mkdir(s.tmpDir, { recursive: true, mode: 0o700 });
      s.proxy = new EgressProxy({
        ...this.proxyOpts,
        allow,
        onBlock: (ev) => {
          s.blocks.push(ev);
          if (s.blocks.length > 500) s.blocks.splice(0, s.blocks.length - 500);
        },
      });
      await s.proxy.listen();
      s.context = await this.chromium.launchPersistentContext(s.profileDir, {
        headless: this.headless,
        args: chromiumArgs(s.proxy.port),
        acceptDownloads: true,
        downloadsPath: s.tmpDir,
        viewport: { width: 1280, height: 800 },
        locale: 'en-US',
      });
      s.context.setDefaultTimeout(15000);
      s.context.setDefaultNavigationTimeout(30000);
      s.context.on('page', (p) => {
        this.wire(s, p);
        s.page = p; // a popup or new tab becomes the page the sprig sees
      });
      for (const p of s.context.pages()) this.wire(s, p);
      s.page = s.context.pages()[0] || (await s.context.newPage());
      this.open.set(sprig, s);
      this.log('browser opened', sprig);
      return s;
    } catch (e) {
      if (s.context) await s.context.close().catch(() => {});
      if (s.proxy) await s.proxy.close().catch(() => {});
      await fsp.rm(dir, { recursive: true, force: true }).catch(() => {});
      if (e instanceof UserError) throw e;
      throw new UserError(`the browser couldn't open: ${e.message.split('\n')[0]}`);
    } finally {
      this.starting--;
    }
  }

  wire(s, page) {
    // No file chooser ever opens: the only uploads are through the type
    // tool's file, from this sprig's downloads.
    page.on('filechooser', () => {});
    page.on('requestfailed', (req) => {
      try {
        if (req.isNavigationRequest() && req.frame() === page.mainFrame()) {
          s.lastNavFailure = { url: req.url(), error: (req.failure() || {}).errorText || '' };
        }
      } catch {
        // the frame went away
      }
    });
    // Over plain HTTP the proxy's refusal is a 403 page, not a failure.
    page.on('response', (resp) => {
      try {
        const req = resp.request();
        if (req.isNavigationRequest() && req.frame() === page.mainFrame() && resp.headers()['x-thicket-blocked']) {
          s.lastNavFailure = { url: resp.url(), error: 'blocked' };
        }
      } catch {
        // the frame went away
      }
    });
    page.on('download', (d) => {
      const p = this.saveDownload(s, d).finally(() => s.pending.delete(p));
      s.pending.add(p);
    });
    page.on('close', () => {
      if (s.page === page && s.context) {
        const rest = s.context.pages().filter((p) => p !== page);
        if (rest.length) s.page = rest[rest.length - 1];
      }
    });
  }

  // saveDownload moves a finished download into the sprig's downloads,
  // within the caps; one growing past the per-file cap is cancelled.
  async saveDownload(s, d) {
    const name = safeName(d.suggestedFilename());
    const watch = setInterval(async () => {
      try {
        for (const f of await fsp.readdir(s.tmpDir)) {
          const st = await fsp.stat(path.join(s.tmpDir, f)).catch(() => null);
          if (st && st.size > this.fileCap) await d.cancel();
        }
      } catch {
        // gone already
      }
    }, 300);
    try {
      const failure = await d.failure();
      if (failure) {
        s.downloads.push({ name, error: failure === 'canceled' ? `bigger than the ${mb(this.fileCap)} limit per file, so it was stopped` : failure });
        return;
      }
      const tmp = await d.path();
      const size = (await fsp.stat(tmp)).size;
      if (size > this.fileCap) {
        s.downloads.push({ name, error: `bigger than the ${mb(this.fileCap)} limit per file` });
        return;
      }
      const used = await dirSize(s.downloadsDir);
      if (used + size > this.downloadsCap) {
        s.downloads.push({ name, error: `the downloads folder is full (${mb(this.downloadsCap)}); nothing more can be saved` });
        return;
      }
      const dest = await uniqueName(s.downloadsDir, name);
      await d.saveAs(path.join(s.downloadsDir, dest));
      s.downloads.push({ name: dest, size });
    } catch (e) {
      s.downloads.push({ name, error: e.message.split('\n')[0] });
    } finally {
      clearInterval(watch);
      await d.delete().catch(() => {});
    }
  }

  // close shuts sprig's browser and saves its profile.
  close(sprig, reason) {
    return this.lock(sprig, async () => {
      const s = this.open.get(sprig);
      if (!s) return false;
      await this.stop(s, true);
      this.log('browser closed', sprig, reason || '');
      return true;
    });
  }

  // discard shuts sprig's browser without saving and deletes its profile.
  discard(sprig) {
    return this.lock(sprig, async () => {
      const s = this.open.get(sprig);
      if (s) await this.stop(s, false);
      await fsp.rm(this.profileFile(sprig), { force: true });
    });
  }

  async stop(s, save) {
    clearTimeout(s.timer);
    this.open.delete(s.sprig);
    await Promise.race([Promise.allSettled([...s.pending]), sleep(5000)]);
    try {
      await s.context.close(); // Chromium writes its cookies out here
    } catch (e) {
      this.log('browser close', s.sprig, e.message);
    }
    await s.proxy.close().catch(() => {});
    try {
      if (save) await vault.save(s.dir, this.profileFile(s.sprig), s.key, s.sprig, this.maxProfile);
    } finally {
      await fsp.rm(s.dir, { recursive: true, force: true });
    }
  }

  async closeAll() {
    await Promise.allSettled([...this.open.keys()].map((sp) => this.close(sp, 'shutdown')));
  }

  async listDownloads(s) {
    const out = [];
    for (const f of await fsp.readdir(s.downloadsDir).catch(() => [])) {
      const st = await fsp.stat(path.join(s.downloadsDir, f)).catch(() => null);
      if (st && st.isFile()) out.push({ name: f, size: st.size });
    }
    return out;
  }
}

function safeName(n) {
  let s = String(n || 'download').replace(/[\x00-\x1f\x7f/\\:*?"<>|‪-‮⁦-⁩]/g, '_').replace(/^\.+/, '_').trim();
  if (!s) s = 'download';
  if (s.length > 120) {
    const ext = path.extname(s).slice(0, 12);
    s = s.slice(0, 120 - ext.length) + ext;
  }
  return s;
}

async function uniqueName(dir, name) {
  const ext = path.extname(name);
  const base = name.slice(0, name.length - ext.length);
  for (let i = 0; i < 1000; i++) {
    const n = i === 0 ? name : `${base} (${i})${ext}`;
    try {
      await fsp.access(path.join(dir, n));
    } catch {
      return n;
    }
  }
  throw new Error('too many files with that name');
}

async function dirSize(dir) {
  let total = 0;
  for (const f of await fsp.readdir(dir).catch(() => [])) {
    const st = await fsp.stat(path.join(dir, f)).catch(() => null);
    if (st && st.isFile()) total += st.size;
  }
  return total;
}

function mb(b) {
  return `${Math.round(b / 1048576)} MB`;
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms).unref());
}

module.exports = { Sessions, UserError, chromiumArgs, safeName };
