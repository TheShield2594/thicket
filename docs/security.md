# Security

What Thicket defends against, what was checked in the security review
(issue #26, November 2026), what changed because of it, and what is still
open. Report a problem privately to the repository owner rather than in a
public issue.

## Threat model

Thicket is a self-hosted server for one household. It holds the
household's conversations, memories and calendars, API keys and chat-bot
tokens (encrypted), and it can act: send messages, run integration (MCP)
tools, run commands in the sandbox, change its own settings.

**Who is trusted**

- The **owner and admins** configure Thicket: settings, integrations,
  permissions, invites. Commands an admin types are what they meant.
- **Members** (adults) use their own sprigs and shared ones. They must not
  reach other people's private sprigs or chats, administer Thicket, or
  use the server to reach places they couldn't reach themselves (the home
  network, cloud metadata).
- **Kids** get kid sprigs only, under the limits their parents set, and
  their details stay out of adults' memory and tools (see
  `docs/household.md`).

**What is not trusted**

- **Outside content**: web pages, search results, PDFs and files, email
  and other integration output, webhook payloads, other sprigs' answers.
  The model reads it, so it can try to steer the model (prompt
  injection). It's wrapped as untrusted data, and code (not the prompt)
  stops it from causing changes the person didn't ask for.
- **The model's choices**: any URL, command or tool argument the model
  picks may come from injected content.
- **The network**: anyone who can reach the public URL, and anything on
  the path to it.
- **Integrations and their servers**: an MCP server chooses what it sends
  back, including its OAuth endpoints.
- **Imported files**: sprig packages, character cards, OpenClaw
  workspaces, backups to restore.

**Out of scope**: someone with a shell on the host or the Docker
socket (they can read the data volume and key), and a compromised model
provider seeing what it's sent.

## How the main defences work

- **Untrusted content and acting** (`internal/agent/taint.go`): a turn
  that read outside content is tainted. In a tainted turn, a tool that
  changes something, or changes Thicket itself (admin and permission
  tools, `Tool.Config`), runs only if the person's own words asked for
  it; otherwise it's held in their inbox. Levels (`read`, `draft`,
  `ask`, `act`) and approvals sit on top. See `docs/architecture.md`,
  "Untrusted content and act".
- **Outbound requests**: `fetch_page` and web search check the address
  when connecting (after DNS, so rebinding doesn't help), on every
  redirect, and refuse loopback, private, link-local, metadata, CGNAT,
  multicast, ULA and NAT64 ranges. Members' calendars and the OAuth
  endpoints of a public MCP server are held to public addresses the same
  way. Addresses deliberately on the home network (model server,
  SearXNG, speech server, MCP servers, admins' calendars) can only be set
  by an admin.
- **Sign-in**: link, invite and session tokens are 32 random bytes,
  stored as SHA-256, single use and short-lived; passwords are Argon2id.
  Cookies are HttpOnly, SameSite=Lax and Secure under https. Cookie
  writes need an `X-Thicket` header (CSRF); dashboard forms carry an
  HMAC token. OIDC checks state, nonce, PKCE (S256), issuer, audience
  and signature algorithms.
- **Secrets**: AES-256-GCM, a random nonce per value, the name as
  associated data; the key lives outside the database (file, environment
  or Docker secret). Known secret values are redacted from logs, the
  activity log, tool output and error messages.
- **The sandbox**: commands run in a separate container without
  Thicket's data or key, as a non-root user with no capabilities, under
  memory, CPU and process limits, reached with a token compared in
  constant time. Each command needs the owner's approval by default.

## What was checked

| Area | Checked | Result |
|---|---|---|
| Sessions and sign-in | token entropy, expiry, single use, storage; cookie flags; session lifetime and fixation; logout and password change; OIDC state, nonce, PKCE, issuer, audience, algorithms; redirects | fine, except: throttling (fixed), app tokens after a password change (fixed) |
| Authorization | admin-only routes and tools, IDOR on chats, sprigs, jobs, approvals; private sprigs in dashboard and trail; kid role | fine, except: admin tools under prompt injection (fixed), unscoped API tokens (open) |
| CSRF and headers | `X-Thicket` header, no CORS, dashboard HMAC tokens; CSP, frame-ancestors, nosniff, Referrer-Policy | fine; HSTS added |
| SSRF | `fetch_page`, web search, SearXNG, model and speech servers, attachments, Telegram files, OIDC, MCP HTTP and OAuth, calendars, avatars, imports | `fetch_page` well guarded; calendars and MCP OAuth discovery fixed |
| Injection | SQL (all parameterised; dynamic parts are allowlisted columns); `html/template` in the dashboard; the web app's markdown; Telegram HTML escaping; Discord mentions (`AllowedMentions` users only) | markdown link rendering fixed |
| File paths | sprig package tar import, backup restore, backup export, skills, onboarding writes, granted paths (`internal/access`), avatar serving, OpenClaw import | fine, except: a symlinked import avatar (fixed) |
| Secrets | cipher, nonces, key file permissions, redaction in logs and `/health`, compose placeholders, `.dockerignore` | fine; the model server's address left `/health` |
| Command execution | stdio MCP servers (scrubbed environment), the sandbox server and token | fine once admin tools are held in tainted turns |
| Resource limits | request body caps, upload caps, webhook limits and signatures, chat and tool rate limits, SSE streams, server timeouts | server read and idle timeouts added; MCP SSE events capped |
| Dependencies | `govulncheck` over modules and the standard library | 40 reachable advisories fixed (toolchain go1.26.11, websocket v1.5.3); now in CI |
| Containers and CI | non-root distroless image, sandbox limits and capabilities, published ports; workflow permissions, untrusted inputs, `pull_request_target` | fine; Thicket service now drops capabilities |

## Fixed in this review

- Admin and permission tools (`settings`, `add_integration` including a
  `stdio` command, `grant_tools`, `set_sprig_access`, `set_tool_level`,
  invites, skills and more) ran straight away in a turn that had read a
  page, PDF or email, so injected text could point `model_url` (and the
  model key) at another server or start a command on the host. They're
  now held unless the person asked; a test keeps every changing admin
  tool covered.
- Sign-in throttling was one household-wide counter, and 5 wrong
  passwords locked a username everywhere, so anyone could lock the owner
  out. Limits are now per client with household-wide backstops.
- App sign-in tokens survived a password change.
- Members could make the server request loopback, LAN and metadata
  addresses through calendar links.
- An MCP server's OAuth discovery could send Thicket to private
  addresses.
- The web app's markdown let a crafted link in a message add HTML
  attributes (no script ran, given the CSP, but a page-covering link
  could).
- An OpenClaw import could copy a symlinked file from outside the
  workspace and serve it as a public avatar.
- No read or idle timeout on the HTTP servers; no HSTS; an unbounded MCP
  SSE event; private sprigs' live state on the public avatar URL; the
  model server's address on `/health`; dashboard invites up to 30 days.
- 39 standard-library advisories (go1.26.0) and one in
  gorilla/websocket.

## Open

- **API token scopes.** CLI, MCP and app tokens share one table with no
  scope. `thicket mcp` asks the server not to let an agent answer
  approvals, but that's a request field, so an agent holding the token
  itself could call `/api/v2` directly. Tokens should carry a kind
  (`cli`, `mcp`, `app`) that the server enforces.
- **The sandbox's network.** It shares the compose network with Thicket
  and has unrestricted outbound access, so an approved command can reach
  Thicket's port, the Docker host and the LAN. Approval per command is
  the guard today; an isolated network with a filtering egress proxy
  would be better.
- **Secret key from a passphrase.** A `THICKET_SECRET_KEY` that isn't a
  32-byte base64 key is hashed with SHA-256, not a slow KDF. Use a random
  key (`openssl rand -base64 32`, or `thicket secrets rotate-key --out
  FILE` to switch to one).
- **The desktop app** is on Electron 33, out of support.
- **Pinning.** Base images and GitHub Actions are pinned by tag, not
  digest or SHA. Dependabot now keeps them current.
- **Read-only containers.** The Thicket container could run with a
  read-only root filesystem (it writes only `/data`, `/sandbox-auth` and
  temporary exports); not yet tested.
- `/avatars/<id>.svg` still confirms whether a sprig id exists, since
  chat apps fetch it without signing in.
