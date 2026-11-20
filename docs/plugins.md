# Plugins: optional abilities in their own containers

Thicket's core bundles no browser, no mailbox and no speech engine.
Abilities like those are **plugins**: each is an MCP server in a container
of its own, started by a docker compose profile, that Thicket connects as
a built-in integration. The [sandbox computer](sandbox.md) is the first
one. A plugin you don't start costs nothing and changes nothing.

## How a plugin is turned on

Two separate steps, both yours:

1. **Start it.** `docker compose --profile <name> up -d` (the sandbox is
   in the default stack). Thicket finds it at `THICKET_<NAME>_URL`, or at
   the plugin's default address if that variable isn't set.
2. **Give it to a sprig.** It's the usual grant: "Juni, give Pip the
   browser" is `grant_tools` with `mcp:<name>/*`. Only whoever may manage
   the sprig can (the person a private sprig belongs to; the owner or an
   admin for a shared one), and it goes in the trail. Tools that change
   something ask for your OK by default, like any integration's.

Until both happen, no sprig can use it. Kid sprigs can't be given most
plugins at all (the sandbox, email, the browser, teaching): that is
refused in code, whatever a character file says.

## How Thicket finds one

For a plugin named `voice`:

| Variable | Meaning |
|---|---|
| `THICKET_VOICE_URL` | its base address, e.g. `http://voice:7071`. Set: Thicket connects it and reports it if it doesn't answer. Set to empty or `off`: it's off. Unset: Thicket tries the plugin's default address quietly. |
| `THICKET_VOICE_TOKEN_FILE` | where the token shared with it lives (default `<data dir>/voice/token`) |

On start Thicket makes a random token (if the file has none), writes it to
the token file and keeps a copy in its encrypted secret store
(`voice_token`), then stores the integration (`voice`, HTTP, `<url>/mcp`,
sending `Authorization: Bearer <token>`). Mount the token file's folder in
both containers, read-only in the plugin's.

A plugin only at its default address is tried for a minute after start
(its container may still be starting). If it never answers it is simply
absent: no error, and an integration left from an earlier start is turned
off (grants stay, so starting it again brings it back).

`thicket doctor` has a `plugin.<name>` line for each known plugin: off,
not running, or its address, whether it accepted the token, its version
and how many tools it has, plus the plugin's own check if it has one.

## Writing one

A plugin lives in `plugins/<name>/` with its own `go.mod` (or
`package.json`), `Dockerfile` and tests, so Thicket's own module doesn't
grow. CI tests each one and builds its image for amd64 and arm64.
[`plugins/hello`](../plugins/hello) is a complete template in about 150
lines of Go with no dependencies.

What a plugin must do:

- **Speak MCP over Streamable HTTP** at `POST /mcp`: `initialize` (with
  `serverInfo.version`, shown by `thicket doctor`), `tools/list` and
  `tools/call`. Plain JSON answers are enough.
- **Answer `GET /health` without a token**: Thicket and `thicket doctor`
  use it to see whether the plugin is running.
- **Require the shared token** on `/mcp`, compared in constant time and
  read from the file on each request (so a new token works without a
  restart). With no token file, refuse everything.
- **Mark read-only tools** with `annotations.readOnlyHint: true`. Every
  other tool asks for approval by default.
- **Treat everything it returns as untrusted.** Thicket wraps a plugin's
  output as outside content: a sprig can't be told what to do by a web
  page or an email the plugin read.
- Run as a non-root user with no added capabilities, publish no port on
  the host, and keep its state in its own volumes.

### Asking first: level floors

A plugin can make its next calls ask first whatever the sprig's
permission level, for something it knows is sensitive (a checkout page),
by adding to a tool result:

```json
{"content": [...], "_meta": {"thicket/floor": {"min_level": "ask", "reason": "payment page"}}}
```

It can only raise a floor, never lower one. See
[Calls that always ask](mcp.md#calls-that-always-ask-level-floors).

### Registering it in Thicket

A plugin Thicket knows is one line in `internal/plugin` (`Builtin`):

```go
plugin.Spec{
	Name:        "voice",            // integration name: mcp:voice/*
	Profile:     "voice",            // docker compose --profile voice
	DefaultURL:  "http://voice:7071", // tried when THICKET_VOICE_URL is unset
	KidsAllowed: false,              // kid sprigs can't be given it
	Exec:        false,              // true if it runs commands (needs exec on)
	Check:       nil,                // optional extra doctor check
}
```

Core gets nothing else from a plugin: no new tables and no special cases.
And in `docker-compose.yml`, a service under its profile:

```yaml
  voice:
    image: ghcr.io/theshield2594/thicket-voice:${THICKET_VERSION:-latest}
    profiles: [voice]
    restart: unless-stopped
    volumes:
      - voice-auth:/plugin-auth:ro
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
```

with `voice-auth:/voice-auth` mounted in the `thicket` service and
`THICKET_VOICE_TOKEN_FILE: /voice-auth/token` set there.

Every schema change a plugin's feature needs in core is a numbered
migration, tested against a fake of the outside service, with a doctor
check and docs, like the rest of Thicket.
