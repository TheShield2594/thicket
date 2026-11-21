# The browser: a sprig's own web browser

A sprig can have its own web browser to look things up on sites you
trust, sign in to an account you set up for it (a store, a library, a
utility), fill in forms and download receipts. It's the
`thicket-browser` plugin: one container that runs a Chromium for each
sprig that is using it, each with its own logins, and an egress proxy
that only lets it reach the sites you allowed. It's off until you start
it and give it to a sprig, and kid sprigs can't have it.

## What you get

- **Only the sites you allow.** Each sprig has a browser allowlist: site
  names like `shop.example`, each allowing its subdomains too
  (`www.shop.example`, `cdn.shop.example`). Everything else is blocked:
  pages, redirects, images and scripts from other sites, downloads. When
  a page it wants is blocked, the sprig is told which site, and that you
  can allow it.
- **Never your network.** Whatever the allowlist says, the browser can't
  reach Thicket, the machine it runs on, your router, your NAS or
  anything else on your LAN, nor cloud metadata addresses. The proxy
  checks the address a name resolves to, and connects to exactly that
  address.
- **Logins that stay.** Each sprig has its own browser profile (cookies,
  saved sessions, its downloads), kept between uses and across restarts,
  encrypted with a key from Thicket's secret store.
- **Downloads, and only those uploaded.** Files the sprig downloads go to
  its own folder (25 MB per file, 50 MB in all). It can attach only those
  to a form; nothing else on any computer.
- **Asks first.** Opening and reading pages runs on its own (only sites
  you allowed); clicking, typing and choosing ask for your OK by default,
  like any integration's tools. You can raise its level for the browser
  once you trust it. A page with a payment form makes its next steps ask
  first whatever its level.
- **Everything it reads is outside content.** A web page can't tell the
  sprig what to do: page text is wrapped as untrusted, like a fetched
  page or an email.

## Setting it up

1. Start it: `docker compose --profile browser up -d`. Thicket finds it at
   `http://browser:7073` (or `THICKET_BROWSER_URL`).
2. Give it to a sprig: "Juni, give Pip the browser" (`grant_tools`
   `mcp:browser/*`). For a shared sprig its owner or an admin grants it;
   for a private sprig, its person.
3. Allow sites: "allow shop.example and library.example in Pip's
   browser" (the `browser_allowlist` tool), the **Browser** page on the
   [dashboard](dashboard.md), or
   `PATCH /api/v2/sprigs/{id}/browser/allowlist` ([API](api-v2.md)). Only
   the sprig's owner changes its list; every change is in the trail, and
   Undo there takes the latest one back. A removal counts from the
   browser's next request, even mid-visit.
4. Sign it in: tell the sprig the account to use ("sign in to the
   library with the card number in my last message"). It types into the
   page; the password field is never read back to it. The login is then
   kept for next time.

You can paste a whole address into the allowlist
(`https://www.shop.example/cart` becomes `www.shop.example`). Addresses
(IPs), `localhost` and local names (`.lan`, `.local`, `.internal`, ...)
are refused: the browser could never reach them anyway. International
names need their `xn--` form for now.

### Settings

On the `browser` service in `docker-compose.yml`:

| | Default | |
|---|---|---|
| `BROWSER_MAX` (or `THICKET_BROWSER_MAX` in `.env`) | 2 | browsers open at once |
| `BROWSER_IDLE_MINUTES` | 10 | an unused browser closes, and its profile is saved |
| `BROWSER_DOWNLOADS_MB` / `BROWSER_DOWNLOAD_FILE_MB` | 50 / 25 | a sprig's downloads folder / one file |

When every browser is busy, another sprig is told so and tries again
later; a sprig's browser closes when it says it's done, or after 10
minutes unused.

### Memory

Budget 300 to 500 MB per open browser, plus about 130 MB for the plugin.
The compose file gives the container 1.5 GB, enough for the default of 2.
On a Raspberry Pi 4 or 5 with 4 GB keep it at 2; with 2 GB set
`THICKET_BROWSER_MAX=1`; with 8 GB, 3 or 4 is fine. The image is built
for amd64 and arm64.

## Tools

| Tool | | |
|---|---|---|
| `open` | runs on its own | opens a page (http or https) and reads it |
| `read_page` | runs on its own | the page's text and the numbered links, buttons and fields on it |
| `click` | asks | clicks one, by its number |
| `type` | asks | types into a field (Enter optional); for a file field, a file from its downloads |
| `select` | asks | picks an option in a list |
| `screenshot` | runs on its own | a picture of the page |
| `back` | runs on its own | the previous page |
| `done` | runs on its own | closes the browser and saves its profile |

## How it works

- **One container, one browser per sprig.** The plugin starts a Chromium
  for a sprig on its first `open` and closes it when it's done, idle for
  10 minutes, or the container stops. Thicket never gets the Docker
  socket: nothing starts containers.
- **The proxy.** Each browser gets its own proxy on `127.0.0.1` inside the
  container, and it is Chromium's only way out: Chromium is started with
  that proxy, without its usual "localhost goes direct" exception, with a
  resolver that resolves nothing itself (so it can't look names up and
  connect around the proxy), QUIC off and WebRTC limited to proxied
  connections. For every request (navigation, each redirect, every image,
  script and download) the proxy checks the host against the sprig's
  allowlist as Thicket sent it on that call, resolves the name once,
  refuses it if **any** address is private, loopback, link-local
  (169.254.169.254), carrier-grade NAT, multicast or reserved (IPv4 and
  IPv6), allows only ports 80 and 443, and then connects to the address
  it checked, so a DNS answer that changes in between can't redirect it
  (DNS rebinding). Allowlist matching happens before DNS: a site that
  isn't allowed is never even looked up.
- **Profiles at rest.** While a sprig's browser is open its profile is a
  folder on a tmpfs (memory). When it closes, the folder (minus caches)
  is packed, compressed and sealed with AES-256-GCM under the sprig's own
  key, which Thicket makes the first time and keeps in its encrypted
  secret store (`browser_<sprig>_profile_key`), sending it with each call;
  the plugin keeps it in memory only while that browser is open. The
  sealed file names the sprig in its authenticated data, so one sprig's
  file can't be swapped in for another's. A file that doesn't open (a
  different key, a changed byte) is left alone, and the sprig is told.
  If the container is killed rather than stopped, what happened since the
  browser opened is lost (the last sealed copy stays).
- **What Thicket sends.** Each call says which sprig is calling (never
  taken from the model), the sprig's allowlist and its profile key
  (`_meta`, see [plugins/browser/PROTOCOL.md](../plugins/browser/PROTOCOL.md)).
  Kid sprigs are refused when granting, and again on every call.

### The network in docker compose

The `browser` service is only on a network of its own (`browser`);
Thicket joins that network too, to reach it. So the browser container
can't reach the sandbox or the mail plugin by name, and Thicket is the
only other container it shares a network with. It still has a route to
the internet (it needs one), and Docker would route its packets to other
private addresses too: what stops that is the proxy, which is the only
thing Chromium can use. The plugin's own code makes no other
connections.

If you want the same rule enforced by the host as well, pin the
network's subnet and drop traffic from it to private ranges in Docker's
`DOCKER-USER` chain, allowing only replies from Thicket:

```yaml
networks:
  browser:
    ipam:
      config:
        - subnet: 172.30.99.0/24
```

```sh
# On the Docker host (as root); Thicket's address on that network is allowed.
iptables -I DOCKER-USER -s 172.30.99.0/24 -d 10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10 -j DROP
iptables -I DOCKER-USER -s 172.30.99.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
```

### Backups

`thicket backup` leaves browser profiles out (logins are easy to make
again, and they're the most sensitive thing a sprig has). To include
them, `thicket backup --browser-profiles` asks the plugin for each sealed
profile and adds it to the archive, still sealed. They open only with
the same secret store key, so keep `THICKET_SECRET_KEY` or `secret.key`
(or use `--include-key`). A browser that is open is backed up as it was
when it last closed. After `thicket restore`, Thicket hands the profiles
back to the plugin when it next starts (and deletes its copies).

## Checking it

`thicket doctor` has a `plugin.browser` line: whether the plugin answers
and accepts its token, that Chromium starts, that its proxy refuses a
host off the allowlist, and that it refuses `169.254.169.254` and
`localhost` even when listed. It needs no internet. It also says how many
browsers are open.
