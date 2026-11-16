# How Thicket works

A short tour for contributors.

## Layout

```
cmd/thicket          CLI: serve, check, healthcheck, export, restore, character, memory, migrate
internal/app         wires everything together (also used by the smoke test)
internal/agent       the engine: routing, prompts, tool loop, approvals, onboarding, initiative
internal/character   character files: parse, validate, load, write without clobbering
internal/memory      memory review (cheap model proposes, code validates) and relevance selection
internal/scheduler   cron and webhook jobs with run history
internal/llm         OpenAI-compatible client (OpenRouter or a local server), retries, fallback model
internal/budget      usage metering and spend caps
internal/mcp         minimal MCP client (stdio, Streamable HTTP) and server registry
internal/channel     channel interface; adapters in channel/discord and channel/telegram
internal/secrets     AES-GCM secret store, log redaction
internal/store       SQLite, versioned migrations, all SQL
internal/settings    chat-changeable settings over env defaults
internal/backup      export, restore, daily snapshots, character import
internal/server      /health, /avatars, /hooks, and /api (mounted from internal/api)
internal/api         local HTTP API: an internal "api" channel plus token auth
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
   - downloads attachments (https only, size-capped): images and PDFs
     go to the model with this turn only (PDFs as `file` parts, parsed
     by OpenRouter per the `pdf_engine` setting; its parse annotations
     ride along on later calls in the turn so the PDF isn't parsed
     twice); text files are redacted, wrapped as untrusted data and added
     to the message. After the reply, each PDF's text (from the parse
     annotations, or a parse-only call to the cheap model when the model
     read it natively) is stored as a `document` message, capped at
     20,000 characters, so follow-up turns can use it. Memory review
     skips documents. A model that refuses images or PDFs gets the turn
     again without them.
3. The turn: system prompt (identity, voice, roster, relevant memories,
   skills, rules), then this conversation's summary if it has one, then
   its newest messages up to the history budget (`history_tokens`;
   automatic: 12,000 tokens on OpenRouter, 3,000 on a local model). A
   conversation ends after `THICKET_IDLE_AFTER` of quiet, so old chats
   don't bleed into new tasks. If the model says its context is full, the
   turn is retried once with half the history, and admins are told.
4. The tool loop: tools are offered by scope and role, and checked again in
   code when called. Risky tools create an approval instead of running.
   Integration output is wrapped as untrusted data. Hard caps: 8 tool calls
   and 6 model calls.
5. Consistency: banned phrases are checked in code; then (if enabled) the
   cheap model compares the reply with the identity. One rewrite on
   failure.
6. The reply is posted as the sprig. Then, if the conversation's
   unsummarised messages are over budget, the cheap model folds the
   oldest into the running summary (`conversations.summary`, up to
   `summary_upto`) until half the budget remains, keeping the last 4
   messages verbatim. Raw messages stay for memory review. A failed
   summary just lets old messages drop out, and isn't retried for 10
   minutes. A handoff then runs the next sprig's turn (at most 2 deep).

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

## Webhook jobs

A job is either `cron` (the scheduler's 20-second tick runs it when due)
or `webhook` (never ticked; `POST /hooks/{token}` starts it through
`Scheduler.Fire`). The token is 24 random bytes; only its SHA-256 is
stored, looked up on each call. The URL is DMed by code straight to the
channel, outside the conversation history, so the model never sees it,
and `thk_…` is a secret pattern, so a pasted URL is intercepted like any
key. `Fire` refuses paused jobs, overlapping runs, a run within a minute
of the last and more than 50 a day; the body (64 KB max, secrets
redacted) is wrapped as untrusted data in the job prompt. Runs are
recorded and reported exactly like scheduled ones.

## Local API

`internal/api` is a channel named `api` that no platform drives: an HTTP
request becomes a `channel.Incoming` with `KnownUser` set from the token,
goes through `Engine.Handle` (the same path as a DM, returning only after
every reply and handoff), and the replies the engine `Send`s to that chat
are collected and returned in the response. One request runs per
conversation at a time. `KnownUser` messages never change where proactive
messages go and can't redeem link codes. Tokens are stored as SHA-256
hashes and made by the CLI. `POST /api/v1/jobs/{id}/run` goes through
`Scheduler.Fire`, with the webhook limits.

## Skills

Skills are rows in `skills` (id and current version) and
`skill_versions` (every version's name, description, steps, note and
author). `SaveSkill` takes the version the change was based on and
refuses a stale one (`VersionConflict`), so concurrent edits never
overwrite each other; restoring copies an old version forward. The
prompt carries an index (first 40 skills); sprigs call `read_skill` for
the steps, which are returned as instructions, not untrusted data. That's
why saving is admin-only and the validator refuses secrets and
`<untrusted>` content. `thicket skill export/import` moves them as
Markdown with YAML frontmatter.

## Model servers

The `model_url` setting (env `THICKET_MODEL_URL`, chat-changeable) is read
on every call. OpenRouter needs its key and gets OpenRouter-only fields
(`usage`, the PDF `plugins`); any other host is treated as a local
OpenAI-compatible server: the key is optional (`model_api_key`), those
fields are left out, calls may take 5 minutes, and PDFs are declined
before download. `Engine.ModelReady` gates the scripted bootstrap: a key
for OpenRouter, a chosen model for a local server. The bootstrap accepts
an address (or "ollama"/"lm studio"), tries `GET /models` on each
candidate, and sets `model_url`, `model`, `cheap_model` and
`fallback_model` (the same model, so no fallback). A model that rejects
tool definitions gets the turn again without tools, and admins are told
once a day.

## Stopping

Messages in a chat take turns behind a lock, so a stop request
(`isStopRequest`: "stop", "cancel", "never mind", 🛑, `/stop`, also after a
sprig's name) is handled before the lock. `Engine.Stop` cancels the
context of the reply in progress in that chat (registered in `turnsIn` by
`handle`; only its user or an admin may stop it) and records the arrival
number, so messages already queued are dropped when they get the lock.
`respond` sees the stop, sends nothing but the acknowledgement (which
lists tools that already ran, from `turn.done`), registers no approvals
and starts no handoff, and records an event so the next reply knows. With
no reply running, a stop cancels the user's running jobs (`jobsRunning`),
whose runs are recorded as "stopped by the user" without a failure
report. Tools that finished stay done: nothing is rolled back.

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
Connected), then register it in `cmd/thicket` alongside Discord and
Telegram, with its token added to the log redactor in `app.New`. Chat
references are stored as `<channel>:<chat id>`. Attachments that need an
authenticated download set `Attachment.Open`, whose errors must not
contain the token (Telegram's file URLs do).

A user can have several identities, one per app account
(`identities` table). `UserByExternal` resolves any of them;
`users.channel/external_id` hold the one the user last wrote from, which
is where proactive messages go. Linking uses a one-time code issued by
`link_account` (DMed by code, held in memory for 15 minutes) and redeemed
by DMing it from the new account. `LinkIdentity` never merges two
histories and never changes a role: an account with its own memories,
jobs or owner role is refused.
