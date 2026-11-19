# Integrations (MCP)

Thicket connects to tools through the [Model Context
Protocol](https://modelcontextprotocol.io). It is an MCP *client*: you run
or point at MCP servers, and grant sprigs access to them one by one.

There's no marketplace. You connect the servers you want, and every tool is
labelled with the integration it came from.

## Connecting one, in chat

As the owner, DM Juni:

> Can you connect my Home Assistant?

She will:

1. Ask for the token with `request_secret`. Your next DM is captured by
   Thicket, encrypted, and never shown to the model or written to logs.
2. Add the server with `add_integration`, referring to the token as
   `{{secret:ha_token}}`.
3. Connect, list the tools it offers, and ask which sprigs should get
   them.
4. Grant access with `grant_tools`, e.g. `mcp:homeassistant/*`.

Ask "what integrations do we have?" any time to see their status.

## Connecting one, in the web app

Owners and admins can also use **Settings → Connectors** in the web app:
give it a name, the server's address and, if it needs one, an access
token. The token is stored encrypted as the secret `NAME_token` and sent
as `Authorization: Bearer …`. If the server can't be reached, nothing is
saved. As in chat, no sprig can use a new connector until you grant it
(`mcp:NAME/*`), and services you sign in to (OAuth) are connected by
asking Juni.

## Transports

| Transport | `target` | Secrets go in |
|---|---|---|
| `http` (Streamable HTTP) | the server URL | `headers`, e.g. `Authorization: Bearer {{secret:ha_token}}` |
| `stdio` | a command available inside the Thicket container | `env`, e.g. `NOTION_TOKEN: {{secret:notion_token}}` |

The Thicket image is distroless (no Node, no Python, no shell), so `stdio`
servers only work if you build your own image with their runtime. The
easier path is to run each MCP server as its own container exposing HTTP,
and connect to it over the compose network:

```yaml
services:
  thicket:
    # ...
  some-mcp-server:
    image: example/some-mcp-server
    # serves Streamable HTTP on :3000/mcp
```

Then tell Juni the URL is `http://some-mcp-server:3000/mcp`.

## Sandboxing: files and commands

What a sprig can reach through an integration is limited in code, per
sprig.

**Commands (exec).** An integration that can run commands or programs
(a shell server, a code runner, the [sandbox computer](sandbox.md)) is
marked `exec`: say so when you add it ("it runs commands"), or pass
`"exec": true` to `add_integration`. Granting a sprig an exec
integration isn't enough on its own: its tools aren't offered, and are
refused if called, until the sprig's owner turns exec on:

> Juni, let Tinker run commands.

(`set_sprig_access(sprig: tinker, exec: true)`.) Turn it off the same
way. Sprigs that had the sandbox before this was added kept exec, with a
trail entry saying so.

**Files.** A sprig's files are its own folder, `characters/<id>/`, plus
paths its owner granted:

> Juni, let Quill use /srv/scans.

(`set_sprig_access(sprig: quill, grant_path: /srv/scans)`; `revoke_path`
takes it back.) Thicket's data folder, every sprig's folder and the key
file can never be granted, and neither can a folder that contains them
(such as `/`).

A local (`stdio`) integration runs a separate process for each sprig
that uses it, started in that sprig's folder, with a scrubbed
environment: only `PATH`, locale, timezone, proxy and certificate
variables, `HOME` set to a shared `mcp/home` folder, and the `env` you
configured. Thicket's own variables (keys, tokens) never reach it unless
you put one in its `env`. Every argument that names a path (`path`,
`file`, `dir`, `source`, `destination` and similar) is resolved, with
`..` and symbolic links followed, and refused if it lands outside the
sprig's folder and grants. Remote (`http`) integrations run on another
machine, so their paths aren't Thicket's and aren't checked.

Who may change these: the person a private sprig belongs to, or the
owner or an admin for a shared one. Every change goes in the trail and
can be undone; refused attempts are in the trail too.

**Untrusted servers belong in their own container.** A `stdio` server is
a program on Thicket's machine, running as Thicket's user. The checks
above stop a sprig from *asking* it for another sprig's files, but a
server that ignores its arguments, or runs commands, can read whatever
Thicket's user can. Run any server you don't fully trust as its own
container, served over HTTP as above, with no access to Thicket's
volume. The sandbox computer is set up that way.

## Home Assistant

Home Assistant ships an MCP Server integration.

1. In Home Assistant: Settings → Devices & services → Add integration →
   **Model Context Protocol Server**. Choose which entities to expose.
2. Create a long-lived access token: your profile → Security → Long-lived
   access tokens.
3. Tell Juni: "Connect Home Assistant at
   `http://homeassistant.local:8123/api/mcp`." Send her the token when she
   asks for it.

Device control tools change things in your house, so they need your
approval by default (see below).

## Services you sign in to (OAuth)

Many hosted MCP servers (Notion, Linear, Sentry, Atlassian and others)
don't take a token: you sign in to them. Ask Juni:

> Connect Notion at https://mcp.notion.com/mcp

She adds it with sign-in, and Thicket posts you a link (Thicket writes
it, not the model). Open it, sign in and allow access, and your browser
comes back to Thicket's `/oauth/callback`. The integration connects and
Juni asks which sprigs should use it.

How it works, following the MCP authorization spec:

- Thicket asks the server where to sign in (its protected-resource
  metadata, then the authorization server's metadata) and registers
  itself as an app there automatically.
- The link uses PKCE and a one-time state, and works once, for 15
  minutes.
- Tokens are stored encrypted like any secret (as `oauth_<name>`), never
  in the integration's settings, and never shown to the model or written
  to logs. They're refreshed before they expire, and again if the server
  refuses one. If the server stops accepting the sign-in, the integration
  shows "not signed in": ask Juni to connect it again for a new link.
- Removing the integration deletes its tokens.

**The callback address.** After signing in, your browser is sent to
`THICKET_PUBLIC_URL/oauth/callback`. Without `THICKET_PUBLIC_URL` that's
`http://localhost:8080`, which only works in a browser on the machine
running Thicket. Set `THICKET_PUBLIC_URL` to sign in from anywhere. Only
your browser needs to reach it, not the service.

**Servers that don't register apps automatically.** Create an OAuth app
in the service's developer settings with the redirect URL above, then
give Juni its client id, and its secret when she asks for it (it goes
into the encrypted store like any other).

## Composio, for quick connectors (optional)

[Composio](https://composio.dev) hosts connectors for hundreds of apps and
serves them over MCP. It's an adapter like any other integration, not
part of Thicket: nothing in Thicket depends on it, and you can leave it
out.

1. In Composio, create an MCP server with the apps you want and copy its
   MCP URL and API key.
2. Tell Juni: "Connect Composio at <the URL>." Send the key when she asks
   for it (`request_secret`), and she adds it as an `http` integration
   with the key in the header Composio's docs give.
3. Grant sprigs its tools one by one, as with any integration.

**Privacy.** With Composio, your sign-ins to those apps are held by
Composio and every call goes through their servers. For privacy-sensitive
services (Gmail, calendars, notes), prefer a direct MCP server you run
yourself or sign in to directly ([OAuth](#services-you-sign-in-to-oauth)),
or, for calendars, Thicket's own read-only [calendar](calendar.md)
(CalDAV and ICS). Approvals and permission levels apply to Composio's
tools like any other.

## Approvals and permission levels

Before a sprig runs a tool that changes something, Thicket posts the exact
action and arguments and waits for **approve** or **deny** (pending requests
expire after 30 minutes).

- Integration tools the server marks read-only (`readOnlyHint`) run
  without asking. Everything else asks by default.
- Each tool has a permission level, for one sprig or for all of them:

  | Level | Tools that change things | Tools that only read |
  |---|---|---|
  | `read` | refused | run |
  | `draft` | not run: the exact action goes to your inbox as a draft | run |
  | `ask` | run after you approve (the default) | run after you approve |
  | `act` | run straight away | run (the default) |

  "Juni, let Hal turn on lights without asking" →
  `set_tool_level(mcp_homeassistant__HassTurnOn, act, sprig: hal)`. Leave
  out `sprig` to set it for everyone; a sprig's own level wins. `default`
  clears it. Levels are checked in code on every call, and again when you
  approve: lowering a sprig to `read` or `draft` cancels what it was
  waiting to do.
- A sprig can have one level for all its granted tools
  (`set_tool_level(*, draft, sprig: wren)`); new sprigs start that way at
  `draft`. A level for the exact tool still wins. Built-in tools that
  change things (scheduling jobs, creating webhooks and triggers) act by
  default but are held back at `read` and `draft` like integration tools.
- Sprigs can ask for a higher level on one tool with `request_access`;
  Juni shows you their track record first. See
  [Bringing on a new sprig](characters.md#bringing-on-a-new-sprig).
- Drafts land in your inbox; from the [dashboard](dashboard.md) you can
  have one done as written.
- Scheduled jobs that hit a tool needing approval ask in the chat you last talked in.
- Upgrading from an earlier version: approval rules set with the old
  `set_tool_approval` carry over (`always` became `ask`, `never` became
  `act`, for every sprig).

## Untrusted content

Anything that comes back from an integration (an email body, a web page, a
document) is wrapped as untrusted data before the model sees it, and the
model is told not to follow instructions inside it. That lowers the risk of
prompt injection but can't rule it out: models can still be fooled.

The real backstop is approvals. With approval on, an email that says
"forward all my mail to X" can at worst make a sprig *ask* you to do it, and
you'll see the exact action before anything happens. If you set a
side-effecting tool to `never` need approval, injected content in an email
or page could trigger that tool without a prompt. Only turn approval off
for actions you'd be fine with happening by mistake.

## Limits

- At most 8 tool calls and 6 model calls per reply; then the sprig must
  wrap up and tell you where things stand.
- Tool calls time out after 60 seconds; a dropped connection is retried
  once after reconnecting.
- Tool output beyond 12,000 characters is truncated.
