# Changelog

All notable changes are listed here. Thicket uses small releases and
versioned database migrations; upgrades never require manual steps unless
stated.

## Unreleased: kill switch, sandboxing, sprig owners, "remember this" and presence (schema v24)

- Relationship memory, apart from facts: how things are going between
  you and each sprig. New kinds `tone` (how you like it to talk to you),
  `likes` and `dislikes` (what annoyed you) sit with the existing
  in-jokes and shared moments; they always belong to one sprig, never
  shared or family memory, so a kid's stays in the kid sandbox. The
  memory review proposes them separately from facts, and the prompt
  gives them to the sprig as "how to be with this person", not facts to
  recite.
- A memory screen: **Memory** in the web app's sidebar and the
  dashboard's memory section show facts and relationship memory apart,
  with where each came from and when. Edit, lock, undo or delete any of
  them, or add your own ("prefers short answers, no emoji"); the next
  reply follows. Edits go through the memory history and undo one by
  one; deleting forgets for good. Parents see and change what a kid
  sprig they set up remembers about its kids. API v2 has the same:
  `GET/POST /api/v2/memories`, `PATCH`/`DELETE /api/v2/memories/{id}`,
  `PUT …/lock`, `POST …/undo`. Migration 0024 records where each memory
  came from and who changed it.
- "Remember this": reply to any message with **remember this** (or say
  "remember this: …"), react with 🧠 or 📌 on Discord or ✍ on Telegram,
  or tap the new brain button in the web app, and it becomes a locked
  memory you set, handled by Thicket itself rather than left to the
  model. The sprig confirms in one line; reply **forget this** to that
  line to undo it. In a family sprig's chat you're asked once "just for
  you, or for the family?". Discord now asks for reaction events (not a
  privileged intent: nothing to change in the developer portal); in
  Telegram groups, reactions reach the bot only if it's an admin.
- Proactive messages go where you are: the app you choose ("reach me on
  Telegram", for every sprig or one), otherwise the app you last wrote
  from, preferring Telegram or Discord over the web app. If that send
  fails, your other linked apps are tried before it waits in the inbox.
  Web-only people are told once that a chat app would bring these to
  their phone.
- A sprig with nothing new stays quiet. Every message it starts itself
  (job and trigger results, check-ins, follow-ups) is dropped if it
  repeats one of its last few messages to you, or if nothing changed
  since it last wrote (no calendar items, nothing due, no new memories or
  job results) and the message brings nothing of its own. Both are
  decided by Thicket, not the model; otherwise the cheap model screens out
  filler. A morning briefing on an empty day sends nothing, never
  "nothing new today". Held-back job runs count as "nothing to report",
  and the activity log says what was held back and why. See
  [Nothing new, nothing sent](docs/characters.md#nothing-new-nothing-sent).
- The memory review now adds household facts said in passing ("we're
  vegetarian now") to family memory, from an adult's conversations with
  family sprigs only; kid and solo sprig chats never write it. Rollback
  undoes it as before.
- `thicket doctor` checks memory: counts per person and sprig, memories
  of people who no longer exist, memories you set that aren't locked,
  and straight after a migration, a comparison with the pre-migration
  snapshot. `thicket update` rolls back a migration that loses memory.
- Sprig packages can carry memory: `export-sprig --memories` (plus
  `--shared`, `--family`, `--user ID`) adds one person's memories with the
  sprig; `import-sprig` lists them and keeps them locked, reviewable or
  not at all (`--memories locked|review|skip`). Off by default; chat
  imports never bring them in. Packages with memories are format 2.

- The kill switch. `thicket pause` stops every reply, job and delegated
  task within a second, and nothing runs (replies, jobs, webhooks,
  triggers, check-ins, follow-ups, held messages, model calls) until
  `thicket resume`. `--sprig ID` pauses one sprig; `thicket pause status`
  shows what's paused. It writes straight into the database, so it works
  without an API token and survives restarts. Also on the dashboard
  (**Pause everything** for the owner, **Pause** on each sprig for its
  owner), at `POST /api/v1/pause` and `/api/v1/resume`, and in chat
  ("Juni, pause Pip"). Messages that arrive get a short "paused" answer;
  webhooks get 503. Every pause and resume is in the trail. See
  [Pausing](docs/operations.md#pausing-the-kill-switch).
- Rate limits: per sprig, 30 tool calls a minute and 20 actions an hour
  taken without asking; per integration, 60 calls a minute. New settings
  `limits.tool_calls_per_minute`, `limits.actions_per_hour` and
  `limits.integration_calls_per_minute`, with per-sprig and
  per-integration overrides. A call over a limit isn't run and goes to
  the owner's inbox.
- Commands need an explicit grant. Integrations that run commands are
  marked `exec` (the sandbox is); their tools are offered only to sprigs
  whose owner turned exec on (`set_sprig_access`). **Sprigs that already
  had the sandbox keep it**, with a trail entry saying so.
- Each sprig keeps to its own files: its folder, `characters/<id>/`,
  plus paths its owner grants. Thicket's data can never be granted. Local
  (`stdio`) integrations now run one process per sprig, started in that
  sprig's folder, with a scrubbed environment (Thicket's keys and tokens
  are no longer inherited; add what a server needs to its `env`), and
  path arguments are resolved, symlinks and `..` included, and checked.
  See [Sandboxing](docs/mcp.md#sandboxing-files-and-commands).
- Sprig owners decide. Members can now change permission levels, exec
  and paths on their own private sprigs, and pause them; family and kid
  sprigs stay with the owner and admins. Attempts on someone else's
  sprig are refused and recorded in the trail. Drafts from a shared
  sprig go to the person who asked, and sprigs are told which memory
  layer (family, shared or their own) each memory comes from. See
  [Who may change a sprig](docs/household.md#who-may-change-a-sprig).
- New guides: [Writing a sprig](docs/authoring.md) and
  [Extending Thicket](docs/adapters.md) (chat adapters and integrations);
  the architecture tour covers ownership, sandboxing, pause, rate limits
  and the trail.
- Avatar moods: `pleased` and `unsure` states. The personality check now
  also reads each reply's mood in the same cheap-model call, and the
  verdict is kept per sprig per conversation (in memory): pleased after a
  reply that landed, unsure after a rewrite for drift, a refusal or hedge,
  or while an approval waits. The mood's face replaces the expression and
  fades back after five minutes. Served at `/avatars/<id>.svg` (live or
  `?state=pleased|unsure`) and on the dashboard; Discord uses
  `<avatar>.pleased.<ext>` / `<avatar>.unsure.<ext>` pictures when they
  exist. No schema change.

## Unreleased: sprig packages and untrusted content

- Sprig packages. `thicket export-sprig ID [-o FILE] [--skill ID]` writes
  one `.sprig` file: the character, avatar settings and picture, voice
  tests, scheduled routines and the skills it uses, with a manifest of
  checksums. Memories, secrets, chats, the trail, the inbox, permission
  levels and integration credentials are never in it (an allowlist of
  paths), and the export is refused if anything looks like a secret.
  `thicket import-sprig FILE` checks the package, shows everything as a
  diff and asks; every tool starts at draft, only `schedule` and `web`
  are kept, a changed skill is added as a new version, and a taken id or
  name is refused with a suggestion. Send a `.sprig` file to Juni in chat
  to import it the same way (new `import_sprig` tool). See
  [Sharing a sprig](docs/characters.md#sharing-a-sprig).
- Untrusted content can't drive act-level actions. Each turn records, in
  code, the outside content that entered it (integration and tool output
  such as email bodies, web pages, files, PDFs, webhook and event
  payloads, other sprigs' answers, handoffs and delegated tasks; webhook
  and event jobs from the start). After that, a tool that changes
  something at `act` runs only if your own words for the turn asked for
  it. Otherwise it goes to your inbox as a draft naming the sources, and
  the trail records it as `held`. See
  [architecture](docs/architecture.md#untrusted-content-and-act).

## Unreleased: adding people, private sprigs and a fuller tour (schema v18)

- "Add my partner's sprig", in one flow. Ask Juni to add someone: she asks
  their name and how they'll chat, and posts one invite link (7 days,
  single use) for the web app, Telegram (the link opens a chat with the
  bot) or Discord (they DM the bot a `join-…` code). New tool
  `invite_person`; the app API's invites take `"own_sprig": true`. When
  they arrive, Juni greets them, not you, and helps them make a sprig of
  their own. See [Households](docs/household.md#adding-someone-start-to-finish).
- Solo sprigs are private from admins too. The owner and admins see that
  someone else's solo sprig exists, its state, whether it logged errors in
  the last day, and its cost, labelled "Kim's private sprig". Its name,
  role, tools, activity summaries and trail entries no longer appear in
  the dashboard, the app API, the live states feed or Juni's reports, and
  admins can't change its tools or levels. Juni tells the owner once,
  plainly, that the database file still holds everything and Thicket
  won't show it.
- A short tour for adults who join later: their name, what's private and
  what's shared, the family sprigs, the calendar, and an offer of a sprig
  of their own. People who joined before this release don't get it.
- Juni's first-run onboarding asks whether Thicket is just for you or for
  a household, and for a household offers to invite the other adults,
  add kids (and set up kid sprigs) and make a first family sprig. It
  introduces the calendar when you mention your schedule (connect one in
  the web app, never in chat), offers the sandbox computer when it's
  connected and a task needs a computer (and later, while no sprig has
  it), and mentions the desktop app once in the web app.
- The Calendar screen offers **Connect your calendar** when nothing is
  connected.
- Background tasks are listed. `thicket jobs`, `GET /api/v1/jobs`
  (`tasks`), `list_jobs` and the dashboard's Jobs section show recent
  delegated tasks and how each ended, including those interrupted by a
  restart.

## Unreleased: quiet hours, calendar removals and PDF limits (schema v17)

- Quiet hours and a daily budget for everything a sprig starts itself.
  Job and trigger results that finish during a sprig's quiet hours, or
  after its `initiative.daily_messages` (default 3 per person per day) is
  used up, are held and delivered when quiet hours end or the next day.
  A message still held after a day goes to the inbox. Approvals, safety
  alerts and failures always get through and say why. Change quiet hours
  or the budget in chat (`set_initiative`); the web app and dashboard
  show today's count and what's held. Jobs set for inside quiet hours
  (a 06:30 reminder, say) now arrive when they end.
- Remove sprig outputs from the Calendar. Each sprig output you may
  remove (your own, or a shared one for adults) has a small **Remove**
  action on the web app's Calendar screen. It goes in the trail like
  `remove_from_calendar`, so undo it under **Changes**. Apps use
  `DELETE /api/v2/calendar/outputs/{event_id}`; calendar items say
  `"removable"`. Kids can't remove anything.
- The owner's setup tour is for the owner and admins only. Members who
  joined later were getting it, with steps that need admin tools; and
  members now get check-ins without having to finish a tour.
- PDF page limits hold for compressed PDFs. Pages kept in compressed
  object streams (as most modern tools write them) are now counted, and a
  PDF whose pages still can't be counted is read only up to 2 MB.

## Unreleased: releases, backups, setup and updates

- Releases. A `vX.Y.Z` tag publishes images for amd64 and arm64 tagged
  `X.Y.Z`, `X.Y` and `latest`, and a GitHub release whose notes are that
  version's section of this file (a tag without a section fails before
  anything is published). Pushes to `main` publish `edge`. The compose
  file takes `THICKET_VERSION` to stay on one version. `thicket
  changelog [VERSION]` prints the notes the binary was built with.
- Backups. `thicket backup` (the new name for `export`) writes the
  database and every file in each sprig's folder (avatar settings were
  missing before). `thicket restore` checks the archive's manifest and
  schema version before touching anything and refuses while Thicket is
  running. Scheduled full backups: settings `backup_dir`,
  `backup_schedule` and `backup_keep`; failures go to the needs-you
  inbox, and Health shows the last backup and its size.
- `thicket settings [list | set NAME VALUE]` changes settings without
  the server.
- `thicket init`: first-run setup at a terminal. It asks for the model
  (an OpenRouter key, or your own server and which of its models), the
  Discord and Telegram tokens, timezone, spend caps and the owner's chat
  account, checks each against the real service, and stores keys and
  tokens encrypted. `--from-env` checks and stores what the environment
  has without asking; `--no-verify` is for a machine that's offline.
  Running it again shows what's set (secrets only as "stored") and
  changes what you type. New setting `timezone`: the household's
  timezone, which new people start with, so Juni doesn't ask.
- `thicket update`: run from the new version with Thicket stopped. It
  shows the changelog since the version that last ran the data, takes a
  full backup, migrates, and runs the doctor's data checks. If a
  migration or check fails, the database files and sprig folders are put
  back byte for byte. It refuses to run under a live server or to go back
  across a schema change. Successful updates are in the trail; every
  attempt is in `update.log` (`thicket update history`). The version that
  last ran a database is now recorded in it.
- README quick start rebuilt on the release image and `thicket init`.

## Unreleased: households, calendar and the app API (schema v16)

- Settings in the web app. Owners and admins get a **Settings** screen
  (the sliders button by your name): models, spending caps and the other
  settings that could only be changed by asking the host, each with a
  reset to its default; and **Connectors**, to add an MCP server by
  address (with an optional access token, stored encrypted) or remove
  one. Services you sign in to (OAuth) are still connected in chat.
- The web app says that setup happens in chat. The home screen has a
  **Set things up with Juni** block (bring on a sprig, schedule a
  routine, connect a service, add a model key); each opens the host's
  chat with the request started for you to finish, and nothing is sent
  for you. On phones it's a **Set things up** row in the sidebar. The
  "No model key" pill is now a button that opens the chat to paste one,
  and Connectors has **Ask Juni** and **Give access** buttons for the
  parts that are done in chat.
- Your account now has your name and timezone, and opens from your name
  in the sidebar as well as the key button.
- A new sprig shows up in the sidebar as soon as it's approved (or its
  file changes), without reloading the page.
- Previews of a sprig that isn't created yet are labelled in the web app
  ("Pippa (preview)") instead of showing its bare id (`web_messages.name`,
  schema v16), and a draft whose look or routines need fixing no longer
  introduces itself twice.
- Households. Roles are owner, admin, member and kid (kids are stored as
  members with `users.kid`). Sprigs have a tier, `household: solo |
  family | kid`: solo sprigs are one person's, family sprigs every
  adult's, kid sprigs talk only to the `kids` listed and are set up by
  `parents`. Kid sprigs are enforced in code: only approved tools that
  read, levels forced to read, no jobs, routines, follow-ups or
  check-ins, and no adult data unless a parent allows `family_memory` or
  `calendar`. Parents read the whole chat (kids can't delete it and
  retention keeps it), get no routine notifications, and get a safety
  alert if a chat touches self-harm, strangers asking for personal
  details, grooming or bullying.
- Family memory: `remember` scope `family` keeps what family sprigs learn
  about the household (`memories.household`), apart from private memory.
- The trail: every permission change (levels, grants, kid access, roles,
  invites) and every sprig calendar entry, with undo. New `trail` table,
  never pruned; a Changes list with Undo buttons on the dashboard.
- Calendar: adults connect calendars read-only (ICS feeds, CalDAV), with
  addresses and passwords in the encrypted store. The view merges
  connected calendars, sprig jobs (scheduled, ran, failed) and sprig
  outputs (each linking to its source), with filters per layer and per
  sprig. New `calendar` scope with `calendar`, `add_to_calendar` and
  `remove_from_calendar`. New tables `calendar_sources`, `sprig_events`.
- Web app screens: **Calendar** (a week at a time, toggles per layer and
  per sprig, connect and disconnect calendars) and, for parents, each kid
  sprig's chats (read-only) with what it may use.
- API v2 (`/api/v2`) for apps: token sign-in with a password or one-time
  link, chats (the web app's threads and event stream), the calendar,
  approvals, the trail, levels, and the household (invites, roles, kid
  sprigs, kid chats for parents, kid access). See docs/api-v2.md.
- Connect Discord or Telegram from chat. An admin sends the bot token to
  Juni in a DM (or says "connect Discord", and she explains where to get
  one): Thicket checks it, connects while running, stores it as
  `discord_token` or `telegram_token` and replies with the link that adds
  the bot. No environment variable and no restart; a token that fails is
  not kept and the working connection is put back. New `connect_chat_app`
  tool (it also adopts a token stored earlier under another name),
  offered as an onboarding step. `request_secret` no longer takes chat
  bot tokens, Discord bot tokens are recognised and redacted like other
  keys, and a refused Discord connection now says whether it was the
  token or the Message Content intent.
- Composio documented as an optional MCP adapter for quick connectors;
  direct MCP servers, OAuth and CalDAV remain for privacy-sensitive
  services.

## Earlier unreleased: pins, deletes, cost visibility, secrets and sign-in (schema v14)

- Desktop app (`desktop/`). An Electron app for macOS, Windows and Linux
  that opens your Thicket server in its own window, with a tray icon
  (closing keeps it running), native notifications that bring the window
  back to the chat, an unread badge, start at login, and microphone access
  for voice messages. Only the server's pages load in it; other links go
  to the browser (the identity provider is allowed during single sign-on).
  The web app calls `window.thicketDesktop` when it's there. The new
  **desktop** workflow checks it and builds unsigned installers, attached
  to a release on `desktop-v*` tags.
- A computer for sprigs. The new `thicket-sandbox` container (Debian
  with git, curl, python3 and friends, a persistent `/workspace`, no
  access to Thicket's data, not published on a port) runs an MCP server
  with `run_command`, `read_file`, `write_file` and `list_files`. With
  `THICKET_SANDBOX_URL` set (compose sets it), Thicket shares a random
  token through the `sandbox-auth` volume and connects it as the
  `sandbox` integration. Nobody can use it until granted
  `mcp:sandbox/*`; commands and writes ask for approval by default.
  Commands time out (60 s, up to 10 min), output is capped, and leftover
  processes are killed. CI builds and publishes
  `ghcr.io/theshield2594/thicket-sandbox`.
- OAuth sign-in for integrations. Hosted MCP servers that need you to sign
  in (Notion, Linear, Sentry, Atlassian and others) can be connected:
  `add_integration` takes `auth: "oauth"`, Thicket finds the server's
  authorization server from its metadata, registers itself, and posts a
  PKCE sign-in link (written by Thicket, never the model). The browser
  returns to the new `GET /oauth/callback`, the tokens go into the
  encrypted store as `oauth_<name>`, and they're refreshed on expiry and
  after a 401. Servers without automatic registration take a client id
  and secret. Removing the integration deletes the tokens.
- MCP server. `thicket mcp` serves the Model Context Protocol on stdio,
  so Claude Desktop, Claude Code, Cursor and other agents can talk to
  sprigs (`ask_sprig`, `list_sprigs`, `list_jobs`, `run_job`, `stop`)
  through the local API with an API token. An agent can never answer an
  approval prompt: the API's new `no_approvals` field, which the MCP
  server always sets, makes Thicket refuse and point the person to the
  dashboard or their chat app.
- Files and voice messages in the web app. A paperclip button (or drag
  and drop) attaches up to 8 files, 30 MB in all; a microphone button
  records a voice message and sends it when tapped again (up to 5
  minutes). `POST /api/chats/{chat}/messages` also takes a multipart form
  (`text` plus `file` parts). Files are held in memory for the reply and
  never stored; the chat shows a line per file.
- Routing. With the new `auto_route` setting on, a message that names no
  sprig goes to the one whose role fits it, picked by the cheap model from
  the sprigs' roles and the last few messages (`route` purpose). The
  current sprig keeps the conversation when the model is unsure, fails or
  names a sprig the person can't use; naming a sprig or writing in its own
  web thread skips routing. Off by default.
- Voice messages. A voice note sent in Discord or Telegram (or any audio
  file: ogg, mp3, wav, m4a, webm, flac, aac) is transcribed and the words
  go into the message, so the sprig answers what was said. Up to 2 per
  message, 10 MB each. New `transcribe_model` setting (default
  `google/gemini-3.8-flash`, which hears audio); transcription is billed
  under the `transcribe` purpose. Needs OpenRouter: with a local model
  server the sprig is told the message wasn't transcribed.
- Pin and delete messages. Reply "pin this" (or say "pin that") to keep a
  message in every reply's context for the rest of the conversation, even
  after compaction; "what's pinned?", "unpin this", "unpin all". Reply
  "forget this" to take a message out of the stored conversation (a
  sprig's is deleted from the chat too); "delete that" deletes the sprig's
  last message. Messages deleted in Discord or the web app leave the
  conversation as well. The web app gets pin and delete buttons. These
  commands are handled in code and never stored. New `message_refs` table
  maps chat-app message ids to stored messages; `messages.pinned`.
- See what history costs. Each chat call records how much of its prompt
  was earlier conversation and kept PDF text (`usage.prompt_chars`,
  `history_chars`, `document_chars`). `thicket costs`, the dashboard and
  the spend tool show the estimated share and dollars ("about 41% of chat
  spend went on re-sending earlier conversation"), and the 80% budget
  warning names `history_tokens` or `document_chars` when lowering it
  would help. New `document_chars` setting (default 20,000, 0 = keep no
  PDF text) replaces the fixed cap.
- Secrets in the encrypted store. `DISCORD_TOKEN`, `TELEGRAM_TOKEN`,
  `OPENROUTER_API_KEY` and `THICKET_MODEL_API_KEY` from the environment are
  stored on start (the environment still wins at startup) and the chat
  apps run on the stored tokens, so a compose file needs no tokens.
  `THICKET_SECRET_KEY_FILE` reads the master key from a file (Docker
  secrets). New `thicket secrets list|set|delete|rotate-key`. Every text
  sink (trail, conversations, inbox, run history, memories, web
  transcripts, prompts, replies, chat sends, `/health`) now passes through
  the redactor, and a pasted known secret is intercepted like a
  credential-shaped one. A test plants each secret and checks every sink.
- Sign in with a password or single sign-on. Local accounts (argon2id),
  made with `thicket user add` or an invite link from the owner (dashboard
  or `thicket user invite`); one-time links still work. Optional OpenID
  Connect (`THICKET_OIDC_*`: issuer, client id and secret stored
  encrypted), matching people by subject, verified email or invite, with
  optional member and admin groups. Sessions have a 90-day hard end, list
  and sign out from the dashboard and the web app, and a password change
  signs out other sessions; wrong passwords lock a username for 15
  minutes. Sprigs can belong to one person (`belongs_to`): members see the
  shared sprigs and their own, in the web app, dashboard, API and chat.
  New `thicket user list|add|passwd|email|invite`. Schema v14.

## Unreleased: sprigs that feel alive (schema v11)

- Skills: a starter pack of six example skills (morning briefing, weekly
  review, inbox digest, trip prep, research, Thicket checkup), added only
  when you ask with `thicket skill starter`. `thicket skill check` checks
  a skill file without saving it. Sprigs write skills in one layout
  (`Needs:`, numbered steps, `Done when:`, `Watch out:`), and saving or
  importing suggests fixes for vague descriptions, unnumbered steps and
  long skills. `docs/skills.md` documents the file format.
- Import characters from other apps: `thicket character import` reads
  character cards (chara_card v1/v2/v3, JSON or PNG), OpenClaw workspaces,
  Hermes Agent homes, Grok personas and plain system prompts, shows the
  sprig before writing it, and with `--memories` brings their memories of
  you as locked user memories. Imports never get tools beyond `schedule`,
  are never the host, and start at `draft`. Juni can also draft a sprig
  from a persona pasted in chat.
- Juni's `import_guide` explains how to bring a character over from a
  named app: the import command (Docker-aware), or for apps with no export
  (Grok, ChatGPT, Character.AI, ...) a prompt to paste into the character
  whose reply Thicket imports directly.
- New sprigs through an approved diff: Juni's interview covers the job,
  personality, voice, tools and schedule; `create_character` shows every
  file the sprig will get (character file, `avatar.json`, voice tests)
  and its routines as a diff, and writes them only when you approve.
  New sprigs start with every granted tool at `draft`.
- Sprig-wide levels (`tool: "*"`), and built-in tools that change things
  (scheduling, webhooks, triggers) are now held back at read and draft.
- Trust upgrades: `request_access` asks for a higher level on one tool;
  Juni presents it with the sprig's track record from the trail.
- Parametric SVG avatars (body, color, leaf hat, accessory, expression)
  with animated idle, thinking, working, sleeping and stuck states.
  `/avatars/<id>.svg`. Juni gets hers.
- Dashboard: roster with live avatars, the needs-you inbox with approve,
  deny, do-it and clear, the trail, and costs split heartbeat/task and by
  model. Its one script is nonce-locked; forms carry a session token.
- The commons (`THICKET_COMMONS_CHANNEL`): sprigs hand off to each other
  by @mention. Every handoff is logged to the trail.
- Event triggers: `create_trigger` watches a read-only integration tool
  for new items (with an optional filter); webhooks can filter bodies.
  Event jobs are held at a spend cap.

## Unreleased: stability (schema v10)

- Permission levels per sprig and tool: `read`, `draft`, `ask`, `act`,
  enforced in code on every call and re-checked on approval.
  `set_tool_level` replaces `set_tool_approval`; existing rules carry over
  (`always` → `ask`, `never` → `act`).
- "Needs you" inbox: failed, interrupted and undelivered jobs and tasks,
  and drafted actions, are kept until resolved (`needs_you` in chat,
  `thicket inbox`). Repeated failures bump one item; nothing is dropped
  when the chat report is de-duplicated.
- Restarts: job runs and delegated tasks in progress when Thicket stopped
  are failed cleanly at startup, put in the inbox and reported once.
  Delegated tasks are now stored (`tasks`).
- Spend is tagged heartbeat or task. `thicket costs [--week|--month|--days
  N] [--json]` shows it by sprig, model and day. At a spend cap,
  heartbeats are skipped (recorded, not failed) instead of failing.
- Integration calls that only read are retried twice with backoff after a
  dropped connection (was once, immediately).
- `thicket doctor`: database, secrets, model server, chat tokens, each MCP
  server, and the running server's live connections. `/health` lists each
  integration as an optional check that never fails it.
- Voice tests: reference exchanges in `characters/<id>/voice_tests/`, five
  bundled for Juni. `thicket voice-check SPRIG [--record]` replays them and
  has the cheap model flag drift in tone, length, catchphrases and role.
  Exported and restored with backups.
- `thicket migrate` lists applied migrations. A test keeps schema changes
  in numbered migration files.
- Tests for levels, the heartbeat-to-task handoff, the inbox, restarts,
  memory remember/list/forget, the budget guard, doctor, costs and voice
  checks. All model calls are faked.

## Unreleased: v0.1.0 (day one)

First version.

- Telegram: a photo album now gets one reply. Its photos arrive as
  separate messages; they're gathered (until none has arrived for 1.5 s,
  5 s at most) and passed on as one message with every photo and the
  caption.
- Web pages behind a proxy: `THICKET_WEB_PROXY=1` sends `fetch_page`
  through `HTTPS_PROXY` / `HTTP_PROXY`. Targets and redirects are looked
  up first and refused unless every address is public; this is weaker
  than the direct connection check, so it's off by default (see
  docs/web.md).
- Discord channel: DMs and server channels, per-sprig name and avatar
  through webhooks, embeds in DMs.
- Telegram channel: Bot API over long polling (no open port), DMs and
  groups, replies labelled with the sprig's name, Markdown converted to
  Telegram HTML with a plain-text fallback, photos and files as
  attachments. Run Telegram, Discord or both; `DISCORD_TOKEN` is no longer
  required on its own.
- Linked accounts: one person can use several apps with one profile.
  `link_account` DMs a one-time code to send from the other app.
  Proactive messages go to the app last written from. Schema v5 adds an
  `identities` table (existing users carry over).
- Chat bot tokens are now redacted from logs.
- Local API and CLI: `POST /api/v1/messages` sends a sprig a message and
  returns its replies (handoffs and approval prompts included); list
  sprigs and jobs; run a job now with an optional payload. CLI:
  `thicket api-token create|list|revoke`, `thicket ask`, `thicket jobs`,
  `thicket run-job`. Tokens act as one user and are stored hashed. Schema
  v6 adds `api_tokens`. Rate-limited messages now report it to the API.
- Skills: a shared library of versioned procedures. Sprigs see an index
  in their prompt and `read_skill` when a task matches; owners and admins
  `save_skill` (sprigs offer after a multi-step task), view history,
  restore and delete with `manage_skill`. Stale edits are refused instead
  of overwriting. Secrets and untrusted content are refused in steps.
  `thicket skill list|export|import` shares them as Markdown. Schema v7
  adds `skills` and `skill_versions`.
- Local models: run on Ollama, LM Studio, llama.cpp or vLLM instead of
  OpenRouter. On the first DM, send Juni the server's address (or
  "ollama"): Thicket checks it, picks a chat model and carries on with
  onboarding. Also `THICKET_MODEL_URL` / `THICKET_MODEL_API_KEY`, and a
  chat-changeable `model_url` setting. Models without tool support still
  answer (without tools, with a notice); PDFs are declined on local
  servers. The compose file maps `host.docker.internal` to the host.
- Context compaction: replies include the conversation's newest messages
  up to a token budget (`history_tokens`, automatic by default) instead of
  a fixed 30 messages. When a conversation outgrows it, the oldest
  messages are folded into a running summary after the reply goes out,
  and later replies see the summary plus the recent messages. A model
  whose context is still too small gets the turn again with half the
  history. Schema v8 adds `conversations.summary` and `summary_upto`.
- Stop: "stop", "never mind", 🛑 or `/stop` while a sprig is working stops
  the reply (and any handoff) at once instead of queueing behind it,
  reports what had already been done, and sets aside messages sent in the
  meantime. Only the person who started it, or an admin, can stop it.
  With no reply running, it stops the user's running jobs. Also
  `POST /api/v1/stop` and `thicket stop`.
- Sprigs working together: `ask_sprig` lets a sprig ask a fellow sprig a
  question mid-reply and use the answer (the asked sprig sees only the
  question and can't ask others in turn); `delegate_task` gives a fellow
  sprig, or itself, a background task whose result arrives by DM (stop
  cancels it; 3 per user at a time).
- Web search: a new `web` tool scope (Juni has it in new installs) gives
  `web_search`, through OpenRouter's web search or your own SearXNG
  (`web_search` and `search_url` settings, `THICKET_SEARCH_URL`), and
  `fetch_page`, which reads pages as text but only from public internet
  addresses (checked at connect time, so redirects and DNS can't get
  around it). Results are treated as data.
- Dashboard: a read-only `/dashboard` with your sprigs, memories, jobs and
  runs, and skills; admins also get spending, health and activity. It
  uses the web app's sign-in (open the web app, then `/dashboard`). No
  JavaScript, strict security headers.
- Webhook signatures: a webhook can require calls signed the way GitHub,
  Gitea/Forgejo or GitLab sign them (`signature` on `create_webhook`, or
  `set_signature` on `update_job`). Thicket generates the secret, keeps
  it encrypted and DMs it; bad signatures get 401 and a notice, repeated
  deliveries aren't run twice, and the event name reaches the sprig. A
  new URL also replaces the secret. Schema v9 adds `jobs.hook_auth`.
- Attachments: sprigs see images (PNG, JPEG, GIF, WebP; up to 4 per
  message, 5 MB each) and read text files (up to 256 KB, first 12,000
  characters). Images go to the model with that message only and are
  never stored. Text files stay in the conversation with secrets redacted,
  marked as untrusted content. If the model refuses images, the sprig
  still replies and says it couldn't see them. Other files are passed on
  by name only.
- PDFs: up to 2 per message (10 MB, 50 pages each), sent to the model with
  that message only. A new `pdf_engine` setting (auto, native,
  cloudflare-ai, mistral-ocr) picks how OpenRouter reads them; within a
  turn, parsed PDFs are reused instead of parsed again.
- PDF follow-ups: a PDF's text (first 20,000 characters, secrets redacted)
  is kept in the conversation, so later questions about it work without
  sending it again. When the model read the PDF itself, a cheap parse-only
  call (free `cloudflare-ai` parser, or `mistral-ocr` if chosen) gets the
  text. Memory review skips document text. Schema v3: messages gain a
  `document` role (the table is rebuilt; a backup is taken first).
- Character system: one Markdown file per sprig with YAML frontmatter;
  validation, hot reload, never overwrites hand edits. Juniper ("Juni")
  ships as the host.
- Conversational onboarding: scripted key setup, then Juni learns your
  name, timezone and needs.
- New sprigs by interview → in-voice preview → tweak → create.
- Self-updating memory: shared and per-sprig scopes, background review by
  a cheap model with code validation, locked user memories, "what do you
  remember" and "forget that", rollback per review run.
- Light initiative: follow-ups, quiet-spell check-ins, quiet hours.
- Handoffs between sprigs.
- MCP integrations over Streamable HTTP and stdio, granted per sprig.
- Approvals for risky actions, configurable per tool.
- Scheduled jobs with run history (ok / nothing found / error / missed).
- Webhook jobs: "make a webhook that…" creates a job started by
  `POST /hooks/<token>`, with the body as untrusted input. The URL is DMed
  privately (only its hash is stored, the model never sees it) and can be
  replaced. Capped at one run a minute and 50 a day per webhook. Schema
  v4 adds `jobs.kind` and `jobs.hook_hash`.
- Spend tracking per sprig and job, daily and monthly caps with alerts.
- Retries and fallback model.
- Encrypted secret store; secrets never reach the model or logs.
- `/health`, structured JSON logs, activity log, failures reported in chat.
- Export / restore, daily snapshots, character import and export.
- Schema migration 0001.
- **Web app.** Every sprig in one place: a thread per sprig, an Everyone
  room, up to three chats side by side, a details panel with memories and
  routines, live typing and replies, light and dark themes. Installable
  as an app (PWA) on phones and desktops.
- One-time sign-in links from `thicket web link`, from Juni in a Discord
  DM (`web_app_link`), or from the startup log on a fresh install.
- Discord is now optional: without `DISCORD_TOKEN` Thicket runs as a web
  app only.
- Schema migration 0002 (web sign-in, sessions and transcripts).
