# Extending Thicket: chat adapters and integrations

**The rule: the core stays small.** The engine does routing, prompts,
memory, permissions, approvals and the trail, and nothing else. A new
capability is one of:

- a **chat adapter**: a new place people talk to sprigs (Matrix, Signal,
  Slack, email). Go code in `internal/channel/<name>`, implementing one
  interface.
- an **integration**: a new thing sprigs can do (a calendar, a smart
  home, a code runner). An MCP server, connected at runtime. No Thicket
  code at all.
- a **skill**: a written procedure sprigs follow ([Skills](skills.md)).

If what you want fits none of these, open an issue before writing code:
it probably belongs outside the core.

## Writing a chat adapter

### The interface

Every adapter implements `channel.Channel` (`internal/channel/channel.go`):

```go
type Channel interface {
	Name() string                                    // "matrix": prefixes every chat reference
	Run(ctx context.Context, h Handler) error        // connect, deliver messages to h until ctx ends
	Send(ctx context.Context, chatID string, p Persona, text string) error
	SendDirect(ctx context.Context, userID string, p Persona, text string) (chatID string, err error)
	Delete(ctx context.Context, chatID, messageID string) error // scrub a pasted secret
	Typing(ctx context.Context, chatID string)                  // best effort
	Connected() bool
}
```

Optional interfaces add features when the platform supports them:

| Interface | What it gives |
|---|---|
| `channel.Poster` (`SendIDs`) | the ids of what a send posted, so "delete that" and pins work on sprig replies |
| `channel.DeleteWatcher` (`OnDelete`) | deleting a message in the app deletes it from Thicket's history |
| `channel.Identity` (`Bot`) | the bot's name and "add me" link, shown when it's connected in chat |
| `channel.Checker` (`Check`) | test a token without connecting, so a bad one is refused before anything changes |

### Normalising messages

`Run` turns each platform message into a `channel.Incoming` and calls the
handler. Get these right and the engine does the rest:

- `Channel` is your `Name()`; `ChatID` is where replies go; `MessageID`
  the platform's id for the message (for replies, pins and deletes).
  The engine stores chats as `<channel>:<chat id>`.
- `UserID` is the platform's stable user id (not a display name, which
  can change), `UserName` a display name. The engine maps the pair to a
  Thicket user (`identities` table) and turns strangers away.
- `IsDM` for private chats. In groups set `Mentioned` when the bot was
  @-mentioned, and `Listening` for chats configured to hear everything
  (like `THICKET_TELEGRAM_CHATS`). Outside DMs the engine answers only
  when one of these is set or a sprig is named.
- `Text` is only what the person typed, with the bot mention stripped.
  Don't add markers for attachments: the engine does.
- `ReplyTo` is the platform id of the message this one answers, if any.
- **Ignore bots and your own messages.** Sprigs post as the bot (or as
  webhooks), so delivering those would make sprigs answer each other
  forever.
- Leave `UserRef` and `Character` at zero values unless your adapter
  authenticates people itself (the web app does).

Call the handler from your receive loop and let it block: messages in one
chat are handled in order, and the engine does its own locking. Recover
from errors with backoff; return from `Run` only when `ctx` ends.

### Attachments

Put files in `Incoming.Attachments`. If the platform gives a plain
`https` URL that needs no credentials, set `URL`, `Name`, `ContentType`
and `Size`; `channel.Fetch` downloads it with size limits and
https-only redirects. If downloading needs the bot token (Telegram's file
URLs contain it), set `Open` instead, and make sure **its errors never
contain the token**: they can end up in logs and chat. The engine
decides what to download and how (images and PDFs to the model, text
wrapped as untrusted data, `.sprig` packages to the importer).

### Personas

Every send names a `Persona`: the sprig's `ID`, `Name`, `AvatarURL` and
`Color`. Show it the best way the platform allows. Discord posts through
a webhook per channel so each sprig has its own name and picture;
Telegram prefixes the message with the name in bold; the web app keeps a
thread per sprig using `ID`. A platform with no way to show names should
still prefix them, so people can tell sprigs apart.

Split long messages at the platform's limit (at a line break, else a
space, and never inside a code block's fence), and from `SendIDs` return every id you posted.

### Registering it

1. Store its token as a secret: add a name in `internal/secrets`
   (like `secrets.TelegramToken`) and a row in `chatApps`
   (`internal/agent/chatapps.go`) with where the token comes from, so
   Juni can connect it in chat.
2. Build it in `cmd/thicket/main.go`'s `NewChannel` switch and in the
   startup loop in `app.New`, next to Discord and Telegram.
3. Add the token to the log redactor (`app.New` does this for stored
   secrets; check any value you log).

### Testing against a fake API

Don't test against the real service. Point the adapter at an
`httptest.Server` that plays the platform: the Telegram adapter has a
`BaseURL` field for exactly this, and
`internal/channel/telegram/telegram_test.go` is a complete example. Its
`fakeAPI` serves queued updates once and records what was sent, which
lets tests check:

- private, group, mentioned and listening chats become the right
  `Incoming` flags, and bots' messages are dropped;
- attachments arrive with working `Open`, and a too-large one is refused;
- a send shows the persona and splits long text;
- errors never contain the token (grep the error strings for it).

For an end-to-end test, `internal/app` tests run the whole engine with a
`fakeChannel` and a `fakeModel` (`app_test.go`): pass your adapter in
`Options.Channels` the same way.

## Adding a capability as an integration (MCP)

Anything sprigs should *do* is an MCP server, in any language. Thicket is
an MCP client: it connects over Streamable HTTP (preferred) or stdio,
lists the tools, and offers them to the sprigs that are granted them.
Nothing in Thicket changes.

1. Write a server with the [MCP SDK](https://modelcontextprotocol.io) for
   your language. Give every tool a clear `description` and an
   `inputSchema`: that's all the model sees.
2. Mark tools that only read with the `readOnlyHint` annotation. Thicket
   treats every other tool as one that changes something: it asks the
   person first by default, and a stray tool can't act on its own.
3. If a tool can run commands or arbitrary programs, say so: the
   integration is then marked `exec`, and only sprigs whose owner turned
   exec on get it ([Sandboxing](mcp.md#sandboxing-files-and-commands)).
4. Run it as its own container serving HTTP, and connect it in chat
   ("Juni, connect http://my-server:3000/mcp") or under **Settings →
   Connectors** in the web app. Tokens go through `request_secret`, never
   in the URL or the chat.
5. Grant it to a sprig (`mcp:my-server/*`, or one tool at a time) and set
   how much it may do on its own with permission levels.

What Thicket does around every call, so your server doesn't have to:
permission levels, approvals, drafts, rate limits, the pause switch, the
trail, timeouts and retries for read-only tools. Tool output is wrapped
as untrusted data, and an action that follows outside content runs only
if the person asked for it ([architecture](architecture.md#untrusted-content-and-act)).
What your server must do: check its own inputs, keep its own secrets,
and not trust that the caller is well-behaved.

Integrations that watch for events (new email, a failing build) need no
extra code either: a read-only listing tool can drive a trigger
([Triggers](triggers.md)), or your service can call a webhook job
([Webhooks](webhooks.md)).

Sharing a sprig that uses your integration: its package lists the scopes
it wants, and the person importing it grants them in chat
([Sharing a sprig](characters.md#sharing-a-sprig)).
