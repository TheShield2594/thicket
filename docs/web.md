# Web search

Sprigs with the `web` tool scope can search the web and read pages:
"what time does the garden centre close today?", "read this article and
tell me what it says about…".

## Which sprigs

Web access is granted per sprig. Juni has it in new installs. To give it
to another sprig, ask Juni: "let Pip search the web" (she adds `web` to
its tools), or add `web` under `tools:` in its character file. On an
install from before web search existed, ask Juni to grant it to herself
too.

It's opt-in because a sprig that can read any address could also be
talked into sending something to one. Everything a sprig reads from the
web is treated as data, never as instructions. If you'd like to approve
every page fetch, ask Juni to set the approval policy for `fetch_page`
to always.

## How it searches

| `web_search` setting | |
|---|---|
| `auto` (default) | SearXNG if `search_url` is set, otherwise OpenRouter's web search when Thicket runs on OpenRouter |
| `openrouter` | OpenRouter searches (via Exa) and the cheap model writes a short answer with numbered sources. Roughly $0.02 per search, plus the cheap model, counted in your spend. |
| `searxng` | Your own [SearXNG](https://docs.searxng.org) at `search_url`: free, private, and works with local models. Sprigs get the result list. |
| `off` | No searching. |

Change these by asking Juni, or set `THICKET_SEARCH_URL` in the
environment for SearXNG. SearXNG must have JSON output enabled: in its
`settings.yml`, add `json` under `search.formats`.

## Reading pages

`fetch_page` reads a page as plain text (scripts, styles and navigation
removed), up to 12,000 characters. It only reaches public internet
addresses: never this machine, your local network, or cloud metadata
addresses, and redirects are held to the same rule. It reads web pages
and text, not PDFs or other files (send those in chat instead). Anything
that looks like a key or password on a page is blanked before a sprig
sees it.

### Behind a proxy

If outbound web traffic on your network has to go through a proxy
(`HTTPS_PROXY` / `HTTP_PROXY`), `fetch_page` can't reach anything by
default: it connects directly so that it can check each connection's
address. Set `THICKET_WEB_PROXY=1` to send page reads through the proxy
named in `HTTPS_PROXY` / `HTTP_PROXY` (and honour `NO_PROXY`).

The trade-off: with a proxy, the proxy makes the connection, so Thicket
can't check the address it actually connects to. Instead it looks the
name up first and refuses it unless every address is public, for the
first request and every redirect. The proxy then looks the name up again,
so a site that changes its DNS between the two lookups could still steer
a fetch to a private address the proxy can reach. Turn it on only if your
proxy can't reach anything you'd mind a sprig reading, or block private
addresses on the proxy itself. Search (SearXNG, OpenRouter) isn't
affected either way.
