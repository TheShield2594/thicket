# Changelog

All notable changes are listed here. Thicket uses small releases and
versioned database migrations; upgrades never require manual steps unless
stated.

## Unreleased: pins, deletes, cost visibility, secrets and sign-in (schema v14)

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
