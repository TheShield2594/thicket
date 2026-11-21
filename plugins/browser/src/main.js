'use strict';
// thicket-browser: sprigs' own web browsers (docs/browser.md). Settings
// come from the environment; see README.md.

const { chromium } = require('playwright');
const { Sessions } = require('./sessions');
const { createServer } = require('./server');
const { version } = require('../package.json');

function num(name, def) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v > 0 ? v : def;
}

function log(...a) {
  console.log(new Date().toISOString(), ...a);
}

async function main() {
  const addr = process.env.BROWSER_ADDR || ':7073';
  const [host, port] = addr.includes(':') ? [addr.slice(0, addr.lastIndexOf(':')) || '0.0.0.0', Number(addr.slice(addr.lastIndexOf(':') + 1))] : ['0.0.0.0', Number(addr)];
  const sessions = new Sessions({
    chromium,
    profilesDir: process.env.BROWSER_PROFILES_DIR || '/var/lib/thicket-browser/profiles',
    runDir: process.env.BROWSER_RUN_DIR || '/run/thicket-browser',
    max: num('BROWSER_MAX', 2),
    idleMs: num('BROWSER_IDLE_MINUTES', 10) * 60000,
    downloadsCapBytes: num('BROWSER_DOWNLOADS_MB', 50) * 1048576,
    fileCapBytes: num('BROWSER_DOWNLOAD_FILE_MB', 25) * 1048576,
    maxProfileBytes: num('BROWSER_PROFILE_MAX_MB', 200) * 1048576,
    log,
  });
  await sessions.init();
  const server = createServer({ tokenFile: process.env.BROWSER_TOKEN_FILE || '/plugin-auth/token', sessions, chromium, version, log });
  server.listen(port, host, () => log(`thicket-browser ${version} listening on ${host}:${port}, at most ${sessions.max} browsers`));
  let stopping = false;
  const stop = async (sig) => {
    if (stopping) return;
    stopping = true;
    log(`${sig}: closing browsers and saving profiles`);
    server.close();
    await sessions.closeAll();
    process.exit(0);
  };
  process.on('SIGTERM', () => stop('SIGTERM'));
  process.on('SIGINT', () => stop('SIGINT'));
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
