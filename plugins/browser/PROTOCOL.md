# thicket-browser: the contract between Thicket and the browser plugin

Thicket core and this plugin talk on the plugin's one port (default
`:7073`), with `Authorization: Bearer <token>` (the shared token file,
read on every request, compared in constant time; no file or a token
under 32 characters refuses everything). Only `GET /health` answers
without a token.

1. **MCP** at `POST /mcp` (Streamable HTTP, plain JSON answers): the tools
   sprigs call.
2. **Admin API** under `/v1/`: what core alone uses (the doctor check,
   open browsers, sealed profiles for `thicket backup`). Sprigs never
   reach it.

## Who is calling: `_meta` on every `tools/call`

```json
{"name": "open", "arguments": {"url": "https://shop.example/"},
 "_meta": {
   "thicket/call": {"sprig": "pip", "approved": false, "draft_only": false},
   "thicket/browser": {"allow": ["shop.example", "bank.example"], "profile_key": "<32 bytes, base64>"}
 }}
```

- `sprig`: whose browser this is. Never taken from `arguments`. Missing →
  tool error. Names are `[a-z0-9][a-z0-9_-]{0,63}`.
- `allow`: the sprig's browser allowlist as it is now (host names, lower
  case). An entry allows that host and its subdomains (`shop.example`
  allows `www.shop.example`, not `badshop.example`). Applied to the
  sprig's proxy before the call runs, so a removal counts at once, also
  for a browser that is already open.
- `profile_key`: the key that seals the sprig's profile at rest (AES-256,
  base64 of 32 bytes). Core keeps it in its encrypted secret store
  (`browser_<sprig>_profile_key`), makes it the first time, and sends it
  on every call. The plugin holds it in memory only while that sprig's
  browser is open (to seal the profile when it closes). A call whose key
  differs from the one the open browser was opened with is refused.

## Tools

| Tool | readOnlyHint | |
|---|---|---|
| `open` {url} | true | opens the browser if needed (cap permitting), loads an http(s) page, returns its reading |
| `read_page` {max_chars?} | true | the open page's text (default 12000 characters, up to 40000) and numbered elements |
| `click` {ref \| selector \| text} | false | clicks, waits for the page to settle, returns the new reading |
| `type` {ref \| selector \| text, value, submit?, file?} | false | fills a field (Enter with `submit`); `file` sets a file field to a file from the sprig's downloads, and nothing else |
| `select` {ref \| selector \| text, option} | false | picks an option by label, else by value |
| `screenshot` {full_page?} | true | PNG (JPEG if over 2 MB) as MCP `image` content, plus a text line |
| `back` | true | history back, then a reading |
| `done` | true | closes the browser now and seals its profile |

Navigation (`open`, `back`) is marked read-only: it only fetches pages
from sites the owner allowed. Everything that acts on a page asks first
by default, like any integration tool.

A **reading** is text: `Page (untrusted content from <host>): "<title>"`,
the URL, the page text (control and bidirectional characters removed),
then `[n] <kind> "<label>"` per visible link, button, field or option
(at most 150; password values are never shown, only `(filled)`), then
any requests blocked while loading and any downloads. Everything returned
is untrusted: core wraps it as outside content.

Tools other than `open` answer "No page is open: use open first." when
the sprig has no browser open.

### Blocked requests

Every request Chromium makes goes through the sprig's egress proxy. A
blocked **navigation** (typed, clicked, redirected) is a tool error
naming the host:

- off the allowlist: `Blocked: evil.example isn't on pip's browser
  allowlist, so nothing was loaded from it. pip's owner can allow it (in
  chat: "allow evil.example in pip's browser", the browser_allowlist
  tool); then try again.`
- a private address (even when listed): `Blocked: nas.lan points at a
  private or local address (10.0.0.5), which the browser may never
  reach. The browser only reaches public internet sites, and this can't be
  allowed.`
- another port than 80 or 443, a name that doesn't resolve: said as such.

Blocked **subresources** don't fail the call; the reading ends with
`Blocked while loading (parts of the page may be missing): cdn.example
(not on the allowlist) ×3, …`.

### Floors

A reading of a page with a payment form (`autocomplete="cc-…"` or a card
number field) carries
`"_meta": {"thicket/floor": {"min_level": "ask", "reason": "payment form on <host>"}}`,
so the sprig's next browser calls ask first whatever its level.

### Downloads and uploads

A download goes to the sprig's own downloads folder, which is part of its
sealed profile. Caps: 25 MB per file (a bigger one is cancelled while it
downloads), 50 MB for the folder. The reading says what was saved or
why not. `type` with `file` is the only upload: the name must be a file
in that folder (no paths). No file chooser ever opens.

## The egress proxy

One per open browser, on `127.0.0.1:<random port>`, the only network
path Chromium has: it is started with `--proxy-server` to it,
`--proxy-bypass-list=<-loopback>` (no direct localhost), a host resolver
that resolves nothing itself, QUIC off, and WebRTC limited to proxied
connections. Per request (plain HTTP, or CONNECT for HTTPS and
WebSockets):

1. the host must be on `allow` (checked before DNS: a host off the list
   isn't even resolved);
2. the name is resolved once; **every** address must be public: no
   0/8, 10/8, 100.64/10, 127/8, 169.254/16 (cloud metadata), 172.16/12,
   192.168/16, 192.0.0/24, documentation and benchmarking ranges,
   multicast, reserved; for IPv6 only 2000::/3, minus NAT64, 6to4, Teredo
   and documentation prefixes (so `::1`, `fc00::/7`, `fe80::/10` and
   IPv4-mapped addresses are refused);
3. the port must be 80 or 443;
4. the connection goes to the address that was checked, never by name
   again (no DNS rebinding).

A refusal is a 403 with `X-Thicket-Blocked: allowlist|private|port|dns|scheme`.

## Lifecycle

- At most `BROWSER_MAX` (default 2) browsers open; another sprig's `open`
  is refused with who has them, until one closes.
- A browser unused for `BROWSER_IDLE_MINUTES` (default 10) closes, as
  does every browser on SIGTERM; closing seals its profile.
- Calls for one sprig run one at a time.

## Profiles at rest

`/var/lib/thicket-browser/profiles/<sprig>.tbp`:
`"TBP1" | nonce (12) | AES-256-GCM ciphertext | tag (16)`, additional
data `thicket-browser-profile:v1:<sprig>` (one sprig's file doesn't open
as another's). The plaintext is a gzipped archive of the profile folder
(Chromium's user data and `downloads/`), minus caches and lock files,
at most `BROWSER_PROFILE_MAX_MB` (200). Written atomically (temp file,
fsync, rename). The open profile lives in `BROWSER_RUN_DIR`, a tmpfs,
which is emptied at start. A file that doesn't open (wrong key, changed
bytes) is left untouched and the call says so.

## Admin API

- `GET /v1/doctor` →
  `{"checks": [{"name": "chromium"|"proxy_blocks_unlisted"|"proxy_blocks_private", "ok": bool, "detail": "…"}], "open": [...], "max": 2}`.
  Starts a throwaway Chromium behind a proxy, loads a host off its
  allowlist (must be refused), and checks that `169.254.169.254` and
  `localhost` are refused even when listed. Needs no internet.
- `GET /v1/sessions` → `{"open": [{"sprig", "opened_at", "last_used"}], "max": 2}`.
- `GET /v1/profiles` → `{"profiles": [{"sprig", "size", "modified", "open"}]}`.
- `GET /v1/profiles/{sprig}` → the sealed file as it is at rest
  (`X-Browser-Open: true` if that browser is open: what it did since it
  opened isn't in it yet). 404 if none.
- `PUT /v1/profiles/{sprig}` (body: a sealed file) → `{}`; 409 while that
  browser is open, 400 if it isn't a sealed profile.
- `DELETE /v1/profiles/{sprig}` → `{}`: closes that browser without
  saving and deletes its profile (logins and downloads).

Errors are `{"error": "…"}`.
