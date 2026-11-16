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

## Notion, Gmail and other OAuth services

v1 does not do OAuth sign-in flows. Hosted MCP servers that *only* accept
OAuth (such as Notion's hosted server) can't be connected directly yet.
Options:

- Use a server that accepts a token: for example run Notion's open-source
  MCP server in its own container with an integration token, and connect
  over HTTP.
- Put an MCP gateway in front that handles the OAuth for you.

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
