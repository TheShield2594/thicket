# How Thicket works

A short tour for contributors.

## Layout

```
cmd/thicket          CLI: serve, check, healthcheck, export, restore, character, memory, migrate
internal/app         wires everything together (also used by the smoke test)
internal/agent       the engine: routing, prompts, tool loop, approvals, onboarding, initiative
internal/character   character files: parse, validate, load, write without clobbering
internal/memory      memory review (cheap model proposes, code validates) and relevance selection
internal/scheduler   cron jobs with run history
internal/llm         OpenRouter client, retries, fallback model
internal/budget      usage metering and spend caps
internal/mcp         minimal MCP client (stdio, Streamable HTTP) and server registry
internal/channel     channel interface; channel/discord is the only adapter in v1
internal/secrets     AES-GCM secret store, log redaction
internal/store       SQLite, versioned migrations, all SQL
internal/settings    chat-changeable settings over env defaults
internal/backup      export, restore, daily snapshots, character import
internal/server      /health and /avatars
```

## A message's journey

1. The Discord adapter normalises the message and ignores bots and webhooks
   (our own sprigs post through webhooks, so this also stops bot-to-bot
   loops).
2. `agent.Engine.HandleIncoming`:
   - resolves the user (owner on first DM, invited users, strangers turned
     away), applies a per-user rate limit, and locks the chat so turns run
     in order;
   - intercepts secrets: a pending `request_secret` capture, or anything
     credential-shaped. These never reach the model or the history;
   - resolves a pending approval if the message is "approve"/"deny";
   - if no model key exists yet, sends the scripted first-run greeting;
   - picks the sprig: addressed by name, else whoever spoke last in this
     chat, else the host.
3. The turn: system prompt (identity, voice, roster, relevant memories,
   rules) plus recent history from this conversation only. A conversation
   ends after `THICKET_IDLE_AFTER` of quiet, so old chats don't bleed into
   new tasks.
4. The tool loop: tools are offered by scope and role, and checked again in
   code when called. Risky tools create an approval instead of running.
   Integration output is wrapped as untrusted data. Hard caps: 8 tool calls
   and 6 model calls.
5. Consistency: banned phrases are checked in code; then (if enabled) the
   cheap model compares the reply with the identity. One rewrite on
   failure.
6. The reply is posted as the sprig. A handoff runs the next sprig's turn
   (at most 2 deep).

## Memory

- Written directly when a sprig calls `remember` (memories the user asked
  for are *locked*: automatic review never changes them).
- Reviewed in the background once a conversation goes idle. The cheap model
  returns JSON operations; `memory.BuildPlan` validates them in code: ids
  must belong to the user, scopes must be participants, content is length-
  and secret-checked, near-duplicates merge, follow-ups need a real future
  date. Each change is logged against the review run so it can be rolled
  back.
- Selected per turn by relevance (word overlap, kind, recency) within a
  size budget, so only what matters goes in the prompt.
- `context` memories expire after 14 days; expired memories are deleted 30
  days later.

## Reliability rules

- Success is checked in code: exit status, API responses, run records. The
  model never grades itself.
- Every scheduled run is recorded as ok, empty ("nothing to report"), error
  or missed (while Thicket was down). Failures are posted by the owning
  sprig; the same error is reported at most every 6 hours.
- Migrations are numbered SQL files applied in a transaction each, with a
  snapshot first. A newer database refuses to run on an older binary.
- Background tasks recover from panics and report them.
- The model is wrapped as: budget guard → router (retries, then fallback
  model) → OpenRouter.

## Adding a channel

Implement `channel.Channel` (Run, Send, SendDirect, Delete, Typing,
Connected), then register it in `cmd/thicket` alongside Discord. Chat
references are stored as `<channel>:<chat id>`, and users are keyed by
`(channel, external id)`, so nothing else needs to change.
