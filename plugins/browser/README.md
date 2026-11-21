# thicket-browser

The browser plugin for Thicket: each sprig that is granted `mcp:browser/*`
gets its own Chromium, with its own persistent (encrypted) profile and an
egress proxy that only lets it reach the sites its owner allowed. What it
does for people is in [docs/browser.md](../../docs/browser.md); the
contract with Thicket core is [PROTOCOL.md](PROTOCOL.md).

```
docker build -t thicket-browser plugins/browser
cd plugins/browser && npm ci && npm test
```

The tests drive a real Chromium (Playwright's) against local test pages.

| Env | Default | |
|---|---|---|
| `BROWSER_ADDR` | `:7073` | listen address |
| `BROWSER_TOKEN_FILE` | `/plugin-auth/token` | the token shared with Thicket, read on every request |
| `BROWSER_PROFILES_DIR` | `/var/lib/thicket-browser/profiles` | sealed profiles, one file per sprig |
| `BROWSER_RUN_DIR` | `/run/thicket-browser` | open profiles (mount a tmpfs here) |
| `BROWSER_MAX` | `2` | browsers open at once |
| `BROWSER_IDLE_MINUTES` | `10` | an unused browser closes after this |
| `BROWSER_DOWNLOADS_MB` | `50` | a sprig's downloads folder |
| `BROWSER_DOWNLOAD_FILE_MB` | `25` | one download |
| `BROWSER_PROFILE_MAX_MB` | `200` | a sealed profile |

## Layout

| File | |
|---|---|
| `src/main.js` | settings, start, shutdown (closes and seals every browser) |
| `src/server.js` | token check, MCP, the `/v1/` admin API and the doctor check |
| `src/tools.js` | the tools, page readings, block messages |
| `src/sessions.js` | one Chromium per sprig: the cap, idle close, downloads, Chromium's flags |
| `src/proxy.js` | the per-sprig egress proxy |
| `src/netcheck.js` | allowlist matching and which addresses are public |
| `src/vault.js` | sealing and opening profiles (AES-256-GCM) |
| `src/pagescript.js` | runs in the page: numbers the elements, reads the text |

## Memory

Budget **300 to 500 MB per open browser**, plus about 130 MB for the
plugin itself. (Measured on amd64 with a simple page: 430 MB summed
resident memory over Chromium's six processes, which counts shared pages
more than once, so real use is lower; heavy sites use more. Not yet
measured on arm64.) With the default cap of 2, the compose file's
`mem_limit: 1536m` leaves room. On a Raspberry Pi 4 or 5 with 4 GB, keep
`BROWSER_MAX` at 2; with 2 GB, set it to 1; with 8 GB, 3 or 4 is fine.
The tmpfs for open profiles counts toward memory too (a profile without
caches is typically a few MB, plus its downloads, at most 50 MB).

## Security

- Chromium runs without its own sandbox (`--no-sandbox`, Playwright's
  default in containers): the container is the sandbox (non-root, no
  capabilities, `no-new-privileges`, memory and process limits).
- Chromium's only network path is the sprig's proxy; the proxy enforces
  the allowlist and refuses private addresses after DNS, dialling the
  address it checked. See PROTOCOL.md, "The egress proxy".
- Profiles are sealed with a key Thicket holds; the plain profile is only
  ever on a tmpfs, and is deleted when the browser closes.
- Page content is untrusted: returned as text, with control and
  bidirectional characters removed; core wraps it as outside content.
