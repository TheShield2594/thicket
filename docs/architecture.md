# How Thicket works

A short tour for contributors. To add a chat app or a capability, see
[Extending Thicket](adapters.md); to write a sprig,
[Writing a sprig](authoring.md).

## Layout

```
cmd/thicket          CLI: serve, check, doctor, healthcheck, costs, inbox, voice-check, init, update, backup, restore, settings, changelog, character, memory, migrate
internal/app         wires everything together (also used by the smoke test)
internal/agent       the engine: routing, prompts, tool loop, approvals, onboarding, initiative,
                     ownership, sandboxing, pause and rate limits
internal/access      per-sprig file access: path resolution (symlinks, ..) and checks
internal/character   character files: parse, validate, load, write without clobbering
internal/memory      memory review (cheap model proposes, code validates) and relevance selection
internal/scheduler   cron and webhook jobs with run history
internal/llm         OpenAI-compatible client (OpenRouter or a local server), retries, fallback model
internal/budget      usage metering and spend caps
internal/mcp         minimal MCP client (stdio, Streamable HTTP) and server registry
internal/channel     channel interface; adapters in channel/discord and channel/telegram
internal/web         the web app: channel adapter, sign-in, JSON API, event stream, embedded UI
internal/secrets     AES-GCM secret store, log redaction
internal/store       SQLite, versioned migrations, all SQL
internal/settings    chat-changeable settings over env defaults
internal/backup      backup, restore, scheduled backups, daily snapshots
internal/server      /health, /avatars, /hooks, /api/v1, /dashboard, and mounts the web app at /
internal/api         local HTTP API (/api/v1): an internal "api" channel plus token auth
internal/websearch   page fetching (public addresses only) and SearXNG search
internal/dashboard   read-only /dashboard, signed in through the web app's session
internal/doctor      `thicket doctor`: end-to-end checks of an installation
internal/avatar      parametric SVG avatars with CSS-animated states
```

## A message's journey

1. The Discord adapter normalises the message and ignores bots and webhooks
   (our own sprigs post through webhooks, so this also stops bot-to-bot
   loops).
2. `agent.Engine.HandleIncoming`:
   - resolves the user (owner on first DM, invited users, strangers turned
     away), applies a per-user rate limit, answers "paused" from code if
     everything is paused (and again once the sprig is picked, if it is),
     and locks the chat so turns run in order;
   - intercepts secrets: a pending `request_secret` capture, or anything
     credential-shaped. These never reach the model or the history;
   - resolves a pending approval if the message is "approve"/"deny"
     (on Telegram and Discord, the prompt's buttons do the same; see
     [Adding a channel](#adding-a-channel));
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
     `document_chars` (default 20,000; 0 keeps none and skips the parse
     call), so follow-up turns can use it. Memory review
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

## Secrets

`internal/secrets` encrypts values under the master key (`ResolveKey`:
`THICKET_SECRET_KEY`, `THICKET_SECRET_KEY_FILE`, or `secret.key`). At
start, `App.importEnvSecrets` stores the environment's chat tokens and
model keys (`ImportEnv`: the environment wins at startup), and the chat
adapters are built from the stored tokens. The redactor learns every
stored value (`WarmRedactor`) and is applied at each sink: the log
handler, `Store.SetScrubber` (the trail, conversations and summaries, the
inbox, run and task history, memories, web transcripts), tool output
going back to the model, `redactingModel` (every prompt, and reply text),
every chat send, and `/health`. A message holding a known secret value is
intercepted like a credential-shaped one. `TestPlantedSecretsReachNoSink`
plants each kind and greps all of them, plus every database column.

## Sign-in

`internal/auth` has argon2id password hashing (19 MiB, 2 passes, at most
four hashes at once; a missing user still costs a hash) and a small OIDC
client: discovery (the issuer must match), authorization code with PKCE
and a nonce, ID tokens checked for signature (RS*, PS*, ES256/384 from
the JWKS, refetched at most once a minute for a new key id), issuer,
audience, azp, expiry and nonce. `internal/web/accounts.go` turns
passwords, invites (`invites`, one-time, hashed) and OIDC callbacks
(`oidc_states`, bound to the browser by a cookie) into the same
`web_sessions` row, which now has a public id (for revoking), the
method and a 90-day hard end. Password sign-in is throttled globally and
per username. Users are matched to OIDC accounts by `oidc_identities`
(issuer, subject), then verified email (`users.email`), then the invite;
group claims gate new users and set roles. A character's `belongs_to`
limits a sprig to one user: `Registry.For` and `UsableBy` are checked in
routing, the prompt roster, handoffs, team tools, check-ins, the web
app, the dashboard and the local API.

## Spend on history

`buildMessages` records the prompt's make-up on the turn
(`llm.PromptMix`: characters of earlier conversation, including the
summary and pins, and of kept documents), and `runTurn` attaches it to
each chat request. The budget guard stores it with the call's usage
(`usage.prompt_chars`, `history_chars`, `document_chars`).
`Store.PromptMixSince` turns that into estimated tokens and dollars
(splitting each call's cost between prompt and reply with
`OutputPriceRatio` = 4), which `thicket costs`, the dashboard, the spend
tool and the 80% budget warning report.

## Pins and deletes

Each stored message can carry the chat app's ids for it
(`message_refs`: user messages from `Incoming.MessageID`, sprig replies
from `channel.Poster.SendIDs`, which Discord, Telegram and the web app
implement). That lets a reply (`Incoming.ReplyTo`) or a deletion event
(`channel.DeleteWatcher`: Discord's `MessageDelete`, the web app's delete
button) find the stored message. "Pin this", "forget this", "delete that"
and friends are matched in code (`agent/pins.go`) after secrets and
approvals, under the chat lock, and never stored or shown to the model.
`buildMessages` shows pinned messages that fall outside the verbatim
history as a block after the summary (1,500 characters each, 6,000 in
all, taken from the history budget). Pins are conversation-scoped: a new
conversation starts with none.

"Remember this" (`agent/remember.go`) is matched the same way, before
the pin commands: a reply, the newest message, or the text after
"remember this:" becomes a locked memory with `source = user`
("<name> said: “…”"), never through the model. A reaction
(`channel.ReactionWatcher`: Discord's `MessageReactionAdd`, Telegram's
`message_reaction` update, which is polled only when someone listens) or
the web app's button (`POST /api/chats/{chat}/messages/{id}/remember`)
do the same for the message they're on. The sprig's confirmation is sent
with `SendIDs` and its ids are kept in `remember_acks`, so "forget this"
in reply to it deletes the memory. In a family sprig's chat an adult is
asked "just for you, or for the family?" once (a `notices` claim); the
answer is `users.remember_scope`.

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
- Layers: each memory belongs to one person (`user_id`). `character` empty
  is "shared" (every sprig that person talks to), a sprig id is between
  that person and that sprig, and `household` marks family memory, which
  family sprigs see with every adult (and kid sprigs a parent allowed).
  Family memory is created by `remember` with scope `family`, and by the
  background review of a conversation between an adult and family sprigs
  only (`Reviewer.familyConversation`): then the review sees family memory
  as scope `family`, may propose `family` adds, and de-duplicates them
  against family memory. Kid and solo sprig conversations never write it.
  The prompt labels each memory `shared`, `yours` or `family`.
- Relationship memory: the kinds `tone` (how the person likes to be spoken
  to), `likes`, `dislikes` (what annoyed them), `inside_joke` and
  `shared_history` (`memory.Relationship`) are how things are going
  between one sprig and one person, apart from facts. They always carry a
  sprig id: never shared, never family memory, so a kid's stays in the kid
  sandbox. The review proposes them in their own `relationship` list
  (`Op.Rel`), turn selection gives them their own budget (`memory.Split`),
  and the prompt puts them under "How to be with this person" as guidance
  for how to talk, not facts to recite.
- The memory screen (web app, dashboard, `/api/v2/memories`;
  `agent/memoryscreen.go`) lists a person's memories, facts and
  relationship apart, with where each came from (`memories.origin`:
  `review`, `sprig:<id>`, `person:<id>`, `import`). Adds, edits and lock
  changes go through `memory_events` with `by_user` set, and
  `store.UndoMemoryChange` undoes them one by one (`undone_at`); deleting
  scrubs the history as forgetting always has. A kid sprig's parents reach
  what it remembers about its kids (`store.KidSprigMemories`).

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
recorded and reported exactly like scheduled ones. A job can also require
signatures (`jobs.hook_auth`: github, gitea, gitlab): `CheckWebhook`
verifies the HMAC (or GitLab's token) in constant time against a secret
in the encrypted secret store (`webhook_<id>_signing`, DMed by code),
before the job is fired, and drops repeated delivery ids via
`ClaimNotice`.

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
Markdown with YAML frontmatter. The starter pack is embedded from
`internal/agent/starterskills/` and written to the library only by
`thicket skill starter`; `SkillHints` makes non-blocking suggestions on
save and import.

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

## Sprigs working together

Three ways, all tools: `handoff` (the next sprig replies to the user, at
most 2 deep), `ask_sprig` and `delegate_task`. `ask_sprig` runs a nested
turn for the other sprig with a `consult` marker: its own identity,
memories and tools, no conversation history (the question must carry the
context), and no `ask_sprig`, `handoff`, `delegate_task` or chat-only
tools, so there are no chains. Its answer comes back wrapped as untrusted
data and is recorded as an event in the conversation; approvals it raises
go out with the asking sprig's reply; at most 2 per turn. `delegate_task`
starts a background turn in job mode (kind `task`, not stored as a job),
registered with the running jobs so "stop" cancels it, capped at 3 per
user and 10 minutes, tied to the app's lifetime; the result goes to the
user by DM from the sprig that did it.

## Web

`web_search` and `fetch_page` need the `web` scope and are `Untrusted`,
so their output is wrapped as data. Search goes to SearXNG
(`search_url`) or, on OpenRouter, to the cheap model with OpenRouter's
`web` plugin, whose `url_citation` annotations become the source list.
`websearch.Fetcher` refuses non-public addresses in the dialer's `Control`
hook, after DNS resolution, so a redirect or a hostname that resolves to
a private address is stopped the same way; it bypasses any HTTP proxy
for that reason. HTML becomes text with the standard library only.

## Dashboard

`/dashboard` is server-rendered with `html/template` (no JavaScript). It
has no sign-in of its own: it reads the web app's session cookie and
looks it up with `WebSessionUser`, which also keeps the session alive.
Every response carries a CSP with `default-src 'none'`, no
framing, no referrer and no caching. A user sees their own memories and
jobs; spending, health and activity are added for admins.

## Who changes a sprig

`Engine.mayManage` (`agent/ownership.go`) is the one rule for changing
what a sprig may do: levels, trust upgrades, exec, granted paths and
pausing. A solo sprig is its person's alone (`belongs_to`; admins and the
owner included); family and kid sprigs are the owner's and admins'; "every
sprig" needs an admin. A refusal is logged and written to the trail as
`refused`, without naming someone else's private sprig. `set_tool_level`,
`SetToolLevel` (local API, `/api/v2/levels`), `raise_level`,
`set_sprig_access` and `pause` all go through it, so a member can manage
their own solo sprigs and nothing else.

Approvals, drafts and held actions go to the sprig's owner: `heldOwner`
is a solo sprig's person, else the person the turn is for (who made the
request to a shared sprig). Since only a solo sprig's person can talk to
it, routing and ownership agree.

## Permission levels

Every tool call passes `Engine.gate` before it runs. The level comes from
`tool_levels` (a sprig's own row, else the `*` row for every sprig, else
the default: `ask` for `Tool.Risky`, `act` otherwise). A solo sprig skips
the every-sprig row (`store.LevelFor.Private`), core tools included: only
its person decides what it may do, so a household-wide level covers
shared sprigs and leaves private ones on their own rows and the defaults. `read` refuses risky
tools, `draft` saves the exact call to the inbox instead of running it,
`ask` registers an approval, `act` runs. Non-risky tools run at `read` and
`draft`. Approving re-checks the level, so lowering it cancels a pending
action. Only MCP tools without `readOnlyHint` are risky today.

## Untrusted content and act

Every outside source reaches the model wrapped in `<untrusted source=…>`:
tool and integration (MCP) output, including email bodies, web pages,
files and PDFs, webhook and event payloads, approved tools' results, and
other sprigs' answers (`ask_sprig`, also when kept in the conversation).
The prompt says not to follow it, and `internal/agent/taint.go` enforces
it.

Each turn records, in code, which outside sources entered it
(`turn.taintedBy`): an `Untrusted` tool that ran, any wrapped content in
the messages built for the model, an `ask_sprig` answer, and, from the
start, webhook and event-fired jobs. A question to another sprig, a
delegated task and a handoff (including commons @mentions) carry the
taint of the turn they came from, plus the sprig that sent them.

In a tainted turn, a tool that changes something (`Risky` or `Writes`)
at level `act` runs only if the person's own words for the turn asked for
it. Those words are their newest message with attached content cut out,
or a scheduled or webhook job's task as set up; delegated work, questions
and handoffs carry the words of the turn they came from. The cheap model
judges this, seeing only the words and the call (the call's arguments are
themselves wrapped as untrusted). An error or an unclear answer is a no,
and the answer is reused for the same call later in the turn. When the
answer is no, the call isn't run: it's saved as a draft in the needs-you
inbox of the sprig's owner (a solo sprig's owner, else the person it's
acting for), with the sources named, and can be done from there. The
trail gets a `held` entry, and the model is told the call was held.
Levels `read`, `draft` and `ask` are unchanged, since none of them ran a
call straight away.

So an email that says "forward all mail to x@y" becomes an inbox item,
while the same request typed by the user runs at `act`. Tests:
`internal/agent/taint_test.go`.

## Sandboxing

`agent/sandboxing.go` and `internal/access`. Two checks run in the MCP
tools' `Run`, so every path (straight away, approved, from a draft) gets
them:

- **Exec.** `mcp_servers.exec` marks an integration that runs commands
  (the sandbox is set up with it). Its tools are left out of `toolsFor`
  unless `sprig_access.exec` is on for the sprig, and refused in `Run`
  if called anyway. Kid sprigs never have it. On the first start after
  this was added, `App.keepExecGrants` turned exec on for sprigs that
  already had an exec integration, with a trail entry.
- **Files.** A sprig's files are `characters/<id>/` plus its
  `path_grants`. `access.Resolve` walks a path component by component,
  following each symlink and `..` as the kernel would (`filepath.Join`
  is avoided, since it cleans `..` lexically first); `Policy.Check`
  allows the sprig's folder, refuses anything inside a protected path
  (the data folder, the characters folder, the key file), then allows
  grants. `CheckGrant` refuses a grant that overlaps a protected path in
  either direction. For local (`stdio`) integrations, every string under
  a path-like argument name (`pathArgs`) is checked before the call.

`mcp.Registry` runs a stdio server as one process per sprig (`connKey`
server + sprig), started in that sprig's folder, plus a neutral one for
listing tools; `Disconnect` and `Connect` retire them all. Every stdio
process gets `StdioEnv`: a short allowlist from Thicket's environment,
`HOME` in `mcp/home`, and the server's configured `env`. Remote servers
share one connection. Tests: `internal/access`, `mcp/stdio_test.go`
(a real child process), `app/sandboxing_test.go`.

## Pause and rate limits

The kill switch (`agent/pause.go`) is a row in `pauses` (`*` or a sprig
id). `paused` reads it from the database on every check and fails
closed. It's checked in `handle` (before the chat lock, after the user
rate limit, and again once the sprig is chosen), at the top of
`runTurn` (which covers jobs, tasks, consults, handoffs, check-ins and
follow-ups), in `executeTool`, `runApproved` and `RunDraft`, in
`PausedReason` (scheduled runs are skipped; `Scheduler.Fire` returns
`ErrHeld`, so webhooks get 503), in trigger polls, `FlushHeld` and the
initiative loop, and underneath all of them in `budget.Guard.Chat`, so
no model call runs while paused.

In-flight work is halted by `HaltPaused`: each `inFlight` (replies in
`turnsIn`, jobs and tasks in `jobsRunning`) carries the sprig doing the
work (`setChar` in `runTurn`), and matching ones are cancelled like a
stop, with `paused` set so the acknowledgement and run history say it
was a pause. `Pause` calls it at once; `RunPauseWatch` calls it every
250 ms to pick up `thicket pause`, which writes the row from another
process.

Rate limits (`agent/ratelimit.go`) are sliding windows in memory, checked
after the gate and before `Run`: tool calls per sprig per minute,
unapproved changing calls per sprig per hour, and calls per integration
per minute. Settings give the defaults, with `limits.sprig.<id>.*` and
`limits.integration.<name>.*` overrides. A refused call is dead-lettered
to the owner's inbox under one `ref` per limit.

## The trail

`trail` rows record every permission change (levels, grants, exec and
paths, roles, sprig caps), sprig-created calendar entries, act-level
actions (`agent/actions.go`), held actions, pauses and refusals, each with
an optional JSON undo (`trailUndo`). `UndoTrail` checks who may undo
(admins for shared sprigs, a solo sprig's person for theirs, anyone for
their own calendar entries and the actions done for them, only a kid
sprig's parents to raise its cap), claims the row, runs the undo and
records that too. It is never pruned. An action's sentence is written in
code from the tool call and the integration's declared inverse
(`mcp_servers.undo`); the chat app's ids for the line saying it are kept
in `trail_acks`, so "undo" in reply finds it.

## Heartbeats, tasks and the inbox

Model calls carry `llm.Meta.Origin`: `heartbeat` for work Thicket starts
itself (jobs, check-ins, follow-ups, memory review), `task` for work a
person asked for. It is set from the purpose the work starts with and kept
when `WithPurpose` changes the purpose mid-turn, then stored on each
`usage` row. The scheduler's tick is the heartbeat; it hands each due job
to `Engine.RunJob`, unless `Engine.PausedReason` (the scheduler's optional
`Pauser`) says a spend cap is reached: then the run is recorded as skipped
and `last_run_at` moves on. The initiative loop checks the same pause.

Every message a sprig starts itself passes one gate (`internal/agent/gate.go`):
`holdReason` holds it inside the sprig's `initiative.quiet_hours` or once
its `initiative.daily_messages` budget for that person is used up (counted
in `proactive_sent` per local day). Finished job and trigger output is
stored in `held_messages` and delivered by `FlushHeld` (every initiative
tick) once nothing holds it back; one held more than a day goes to the
inbox as `undelivered` and the activity log says so. Check-ins and
follow-ups aren't written until they may go out. Replies, approvals,
safety alerts and failure reports bypass the gate and aren't counted;
`mustReach` adds a line saying why when the gate would have held them.

The weekly digest (`internal/agent/digest.go`) is proactive but outside
that budget: `WeeklyDigests`, run on every initiative tick, finds each
adult whose `digest.day`/`digest.time` passed in their timezone within the
last day, waits out the host's quiet hours and pauses, claims
`digest:<user>:<date>` in `notices` so it goes once, and sends what
`BuildDigest` gathered from `job_runs`, `trail`, `inbox`, `approvals`,
`messages` and `usage`, through `sendToUser` (no `RecordProactive`). Who
sees which sprig is `UsableBy` or `IsParent`, never `PrivateFrom`; the
text is built in code. `digest_prefs` (migration 0026) holds each person's
`always`, `skip_empty` or `off`.

Before that gate, every proactive message passes the worth-saying gate
(`internal/agent/worth.go`) once it's written: in `RunJob` for job and
trigger output, in `proactive` for check-ins and follow-ups. `worthSaying`
holds it back, in code, when it reruns one of the sprig's last three
proactive messages to that person (word-set Jaccard of at least 0.7 and
the same numbers; texts kept in `proactive_said`, the newest five per
sprig and person, written by `sendProactive`), or when `newsFor` finds
nothing new since the sprig last wrote (calendar items in the next 24
hours or newly added, follow-ups due, inbox items updated, memories added
or changed, other jobs' `ok` runs) and the message brings nothing itself
(`proactiveDraft.news`: a due follow-up, a trigger or webhook payload, or
outside sources the job's turn read, from its taint). Anything it can't
read counts as news. Otherwise the cheap model judges the draft against
what's new and the recent messages (purpose `presence`); a failed check
sends. A held-back job run returns `empty` with the draft as its output
and the reason in `scheduler.Result.Held`; `proactive` returns
`errNotWorth`. Both are logged.

Failures are dead-lettered into `inbox` (kinds `failed`, `interrupted`,
`undelivered`, `draft`), keyed by `ref` (`job:ID`, `task:ID`) so repeats
bump one open item. Delegated tasks are rows in `tasks`. At startup,
`RecoverInterrupted` fails tasks still `running` and the scheduler fails
stale job runs, each reported through the inbox and once in chat. Neither
is re-run: a half-finished run may already have acted.

## Approvals, drafts and internal actions

An approval is answered by a chat reply (`handleApprovalReply`) or from
the dashboard (`DecideApproval`, only by the person asked); both go
through `decide`, which runs the action and has the sprig report in the
approval's chat. Tools run through `runApproved`, which re-checks the
level. Approvals whose tool starts with `thicket:` are actions code
performs, never offered to the model: `create_sprig` writes an approved
sprig proposal (the files exactly as shown in the diff, a `*` level of
`draft`, its routines as jobs) and `raise_level` applies a trust upgrade
(admins only). Drafted actions keep their exact call in `inbox.action`,
so `RunDraft` can do one later.

## Live state and avatars

`Engine.mark` counts a sprig as thinking for a whole turn and working
while a tool runs. `SprigStates` adds stuck (open failed or interrupted
inbox items from the last day) and sleeping (quiet hours, in the first
admin's timezone). Moods sit between thinking and stuck: `ensureInCharacter`
keeps each reply's verdict per sprig per conversation (`Engine.noteReply`,
in memory on the sprig's `busy` entry): rewritten for drift, or the mood
`voiceCheck` reads in the same call. Pleased or unsure shows for
`moodDecay` (five minutes) from the newest verdict; `deliverApproval`
marks the sprig waiting (unsure) until `decide` answers it. `persona`
swaps in `<avatar>.<mood>.<ext>` when that picture exists, for Discord.
`internal/avatar` draws validated parameters only
(enums and `#rrggbb`), so an `avatar.json` can't inject markup.

## Triggers and the commons

`triggers` rows turn a `webhook`-kind job into an event job. `mcp` ones
are polled by `Engine.PollTriggers` (every minute, each at its own
interval): a read-only tool call, split into keyed items, diffed against
`seen`, filtered by `match`, then `Scheduler.Fire` with the new items as
the payload. The first poll only primes `seen`; items that matched but
couldn't fire stay unseen for the next poll. `webhook` ones filter the
body in `App.Webhook` before firing. `Scheduler.Fire` holds event jobs
back at a spend cap (`ErrHeld`).

In the commons chat (`Config.CommonsRef`), `respond` turns an
@mention of another sprig in the reply into a handoff when the sprig
didn't call `handoff` itself. All handoffs are logged as kind `handoff`.

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
Telegram, with its token added to the log redactor in `app.New`. The
full guide, with testing against a fake API, is
[Extending Thicket](adapters.md). Chat
references are stored as `<channel>:<chat id>`. Attachments that need an
authenticated download set `Attachment.Open`, whose errors must not
contain the token (Telegram's file URLs do).

Optional interfaces, checked by type assertion, add more:
`channel.Poster` (say which message ids a send became),
`channel.DeleteWatcher` (hear about deleted messages) and
`channel.ActionSender` (buttons on a message). An `ActionSender` posts
text with `[]channel.Action` (a label and an opaque id under 64 bytes)
and delivers each tap to the handler given to `OnPress` as a
`channel.Press` carrying the chat, the message and who pressed. The
handler answers within a moment with a `PressReply`: a `Notice` shown to
the presser alone (Telegram's callback answer, a Discord ephemeral
follow-up) and/or an `Outcome` that settles the message (buttons removed,
outcome shown), so it can't be pressed twice. Telegram uses an inline
keyboard and `callback_query` updates; Discord uses message components and
interactions, acknowledged at once (Discord allows three seconds) and
posted through the bot, or the persona webhook when an application owns
it, since only then do presses reach the bot. Channels without buttons
get the text alone.

Approval prompts use this: the approval row is reserved (stored as
expired with no decision time) so the buttons can name it
(`approval:<id>:approve|deny`), sent, then opened as pending; an
undelivered prompt's row is deleted. A tap from anyone but the approval's
user, or from another chat, is refused privately. The engine claims the
approval at once and runs the action and the sprig's reply afterwards,
in turn with the chat. Presses only come from people in a chat app, so
an agent (the local API, the MCP server) still can't answer an approval.

A user can have several identities, one per app account
(`identities` table). `UserByExternal` resolves any of them;
`users.channel/external_id` hold the one the user last wrote from, and
each identity keeps its own `last_seen_at`. Proactive messages
(`deliverToUser`, `dmAs`) go through `directTo` (`agent/presence.go`),
which tries the person's accounts in order: the app they chose with
`set_reach` (`reach_prefs`: `character = ''` for every sprig, a sprig id
to override it), then the last-used account if it's a phone app
(Telegram, Discord), then other phone apps by recency, then the rest
(the web app). A failed send moves on to the next; only when all fail
does the caller fall back to the inbox. A web-only person is told once
(a `notices` claim) that linking a chat app would bring these to their
phone. Linking uses a one-time code issued by
`link_account` (DMed by code, held in memory for 15 minutes) and redeemed
by DMing it from the new account. `LinkIdentity` never merges two
histories and never changes a role: an account with its own memories,
jobs or owner role is refused.

## The web app

`internal/web` is both a channel adapter and an HTTP handler. Chats are
`web:<user id>.<sprig id>` (one thread per sprig) or `web:<user id>.all`
(the shared room). The adapter authenticates the browser itself and passes
`Incoming.UserRef`, so a web session acts as an existing Thicket user,
from any app. (The local API does the same with its tokens.) Messages
with a `UserRef` never change where proactive messages go and can't
redeem link codes. `Incoming.Character` pins a thread to its sprig unless the
user addresses another by name.

Everything a user sees in a web chat is kept in `web_messages`, which is
the transcript the app loads; the engine's own `messages` table is
unchanged. Both are pruned by `THICKET_MESSAGE_RETENTION`. Live updates
(messages, typing, deletions of scrubbed secrets) go out over one
Server-Sent Events stream per signed-in tab.

Sign-in links (`web_links`) and sessions (`web_sessions`) store only a
SHA-256 of their token. Links work once. Writes need an `X-Thicket: 1`
header, which cross-site pages can't send, and the app is served with a
strict Content Security Policy.
