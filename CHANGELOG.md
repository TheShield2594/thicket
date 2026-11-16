# Changelog

All notable changes are listed here. Thicket uses small releases and
versioned database migrations; upgrades never require manual steps unless
stated.

## Unreleased: v0.1.0 (day one)

First version.

- Discord channel: DMs and server channels, per-sprig name and avatar
  through webhooks, embeds in DMs.
- Telegram channel: Bot API over long polling (no open port), DMs and
  groups, replies labelled with the sprig's name, Markdown converted to
  Telegram HTML with a plain-text fallback, photos and files as
  attachments. Run Telegram, Discord or both; `DISCORD_TOKEN` is no longer
  required on its own.
- Linked accounts: one person can use several apps with one profile.
  `link_account` DMs a one-time code to send from the other app.
  Proactive messages go to the app last written from. Schema v4 adds an
  `identities` table (existing users carry over).
- Chat bot tokens are now redacted from logs.
- Local API and CLI: `POST /api/v1/messages` sends a sprig a message and
  returns its replies (handoffs and approval prompts included); list
  sprigs and jobs; run a job now with an optional payload. CLI:
  `thicket api-token create|list|revoke`, `thicket ask`, `thicket jobs`,
  `thicket run-job`. Tokens act as one user and are stored hashed. Schema
  v5 adds `api_tokens`. Rate-limited messages now report it to the API.
- Skills: a shared library of versioned procedures. Sprigs see an index
  in their prompt and `read_skill` when a task matches; owners and admins
  `save_skill` (sprigs offer after a multi-step task), view history,
  restore and delete with `manage_skill`. Stale edits are refused instead
  of overwriting. Secrets and untrusted content are refused in steps.
  `thicket skill list|export|import` shares them as Markdown. Schema v6
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
  history. Schema v7 adds `conversations.summary` and `summary_upto`.
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
  runs, and skills; admins also get spending, health and activity. Sign
  in with a one-time link from Juni (`dashboard_link`) or
  `thicket dashboard-link`; sessions last a week. No JavaScript, strict
  security headers. Schema v8 adds `web_sessions`.
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
  text. Memory review skips document text. Schema v2: messages gain a
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
  v3 adds `jobs.kind` and `jobs.hook_hash`.
- Spend tracking per sprig and job, daily and monthly caps with alerts.
- Retries and fallback model.
- Encrypted secret store; secrets never reach the model or logs.
- `/health`, structured JSON logs, activity log, failures reported in chat.
- Export / restore, daily snapshots, character import and export.
- Schema migration 0001.
