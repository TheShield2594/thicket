# Thicket

A small, self-hosted family of AI companions. Its characters are called
**sprigs**. They live in your Discord or Telegram, remember you, take a
little initiative, and stay true to who they are.

Thicket is one Go binary and one SQLite file. No vector database, no
bundled browser, no plugin marketplace. You set it up by talking to Juni,
the first sprig.

- **Characters that feel alive.** Each sprig is one Markdown file: name,
  personality, voice rules, relationships, tools. Each keeps its own
  memories of you, follows up on things you mentioned, and knows the
  others. A personality check runs before each reply goes out.
- **Memory that maintains itself.** When a conversation goes quiet, a cheap
  model reviews it and proposes memory changes. Code validates every change
  before it is written: duplicates merge, small talk expires, and memories
  you set yourself are never touched. Ask "what do you remember about me?"
  or "forget that".
- **Conversational setup.** First-run onboarding, new sprigs, integrations
  and scheduled jobs are all set up in chat. You don't edit config files.
- **Safe by default.** Secrets are encrypted and never reach the model or
  the logs. Risky actions ask for your OK. Tool output is treated as data,
  not instructions. Spend caps, model fallback, loop limits.
- **Fails loudly.** When something breaks, a sprig tells you in chat. Job
  history tells "nothing found" apart from "broke".

## Quick start

1. **Create a bot** on Discord, Telegram, or both.
   - **Discord:** at <https://discord.com/developers/applications>, make an
     application, add a bot, and switch on **Message Content Intent**. Copy
     the bot token. Invite it to your server with the `bot` scope and these
     permissions: Send Messages, Read Message History, **Manage Webhooks**
     (so each sprig posts with its own name and avatar).
   - **Telegram:** message [@BotFather](https://t.me/BotFather), send
     `/newbot`, and copy the token. See [Telegram](docs/telegram.md).
2. **Run it.**

   ```bash
   DISCORD_TOKEN=your-bot-token docker compose up -d
   ```

   (or `TELEGRAM_TOKEN=…`, or both). In Portainer: Stacks → Add stack →
   paste `docker-compose.yml` → add `DISCORD_TOKEN` and/or `TELEGRAM_TOKEN`
   as environment variables → Deploy. This pulls
   `ghcr.io/theshield2594/thicket:latest`, which CI publishes on every push
   to `main`. (If the package is private, make it public in the repo's
   Packages settings or log Portainer in to ghcr.io.) The optional
   variables listed in `docker-compose.yml` can be added the same way.
   `THICKET_DATA_DIR` and `THICKET_HTTP_ADDR` are fixed inside the image
   (`/data` and `:8080`): change the volume or port mapping in the compose
   file instead.
3. **DM the bot** (on Telegram, open the bot and press Start). Juni says hello and asks for an
   [OpenRouter](https://openrouter.ai/keys) key. Paste it in the DM: she
   checks it works and stores it encrypted. Or, to run on your own
   hardware, send her the address of your Ollama or LM Studio server
   instead ([Local models](docs/local-models.md)). Then she'll learn your name,
   your timezone, and what you'd like help with.

The first person to DM the bot becomes the owner, unless you set
`THICKET_OWNER_DISCORD_ID` or `THICKET_OWNER_TELEGRAM_ID`. Using both apps?
Ask Juni to link the other one: she DMs you a one-time code to send from
there, and you keep one profile and one set of memories.

Not sure something's right? `docker compose run --rm thicket check` says
exactly what's missing, and `GET /health` reports each part.

## Talking to sprigs

- In DMs, just talk. The sprig you spoke to last keeps the conversation.
- Address someone by name to switch: `Pip, what's on tomorrow?`
- In server channels, sprigs answer when you @-mention the bot or start
  with a sprig's name. List channel ids in `THICKET_DISCORD_CHANNELS` to have
  them answer everything there. Telegram groups work the same way, with
  [one setting to change](docs/telegram.md#groups) for addressing by name.
- Ask Juni to bring someone on: "I need help keeping on top of my
  calendar". She interviews you, the new sprig introduces itself as a
  preview, you tweak it, and the character file is written for you.
- Send pictures, PDFs and text files. Sprigs look at images (the chat
  model needs vision, as the defaults have), read PDFs (up to 2 per
  message, 10 MB and 50 pages each) and text files such as notes, CSVs or
  logs. Images are seen for that reply only. A PDF's text (its first
  20,000 characters) is kept for follow-up questions until the
  conversation goes quiet; text files stay in the conversation like any
  message. Other files (Word documents, for example) are passed on by
  name only.
- PDFs are read by the model itself when it can, otherwise by OpenRouter's
  OCR ($2 per 1,000 pages). Ask Juni to set `pdf_engine` to
  `cloudflare-ai` (free, text only), `mistral-ocr` (best for scans) or
  `native`; the default is `auto`.
- Schedule things: "Every weekday at 7:30, give me a summary of my day."
- Let other things wake a sprig: "Make a webhook that tells me when a CI
  build fails." Thicket DMs you a private URL; anything that POSTs to it
  starts the job with what it sent. See [Webhooks](docs/webhooks.md).
- Teach them once: after working through a task, a sprig offers to save it
  as a skill, a versioned procedure every sprig follows from then on. See
  [Skills](docs/skills.md).
- Script it: `thicket ask --sprig pip "what's on tomorrow?"` prints the
  reply, and `thicket run-job 4` runs a job now. Both use a local HTTP API
  with tokens from `thicket api-token create`. See [Local API](docs/api.md).
- Risky actions (sending email, controlling devices) show the exact action
  and wait for you to reply **approve** or **deny**.
- Changed your mind? Say **stop** (or "never mind", 🛑, `/stop`) while a
  sprig is working: it stops at once, tells you anything it had already
  done, and sets aside messages you sent in the meantime. With no reply
  running, "stop" stops your running jobs.

## Configuration

Everything has a sane default. Settings marked *chat* can also be changed
by asking Juni (as the owner), and the chat value wins.

| Variable | Default | |
|---|---|---|
| `DISCORD_TOKEN` | one of these | Discord bot token |
| `TELEGRAM_TOKEN` | is required | Telegram bot token ([details](docs/telegram.md)) |
| `THICKET_OWNER_DISCORD_ID` | first DM | your Discord user id |
| `THICKET_OWNER_TELEGRAM_ID` | first DM | your numeric Telegram user id |
| `THICKET_ALLOWED_USERS` | | comma-separated user ids allowed in, plain or as `telegram:ID` / `discord:ID` (or ask Juni to invite someone) |
| `THICKET_DISCORD_CHANNELS` | | Discord channel ids where sprigs answer every message |
| `THICKET_TELEGRAM_CHATS` | | Telegram group ids where sprigs answer every message |
| `OPENROUTER_API_KEY` | | optional; Juni can collect it in a DM |
| `THICKET_MODEL_URL` *chat* | OpenRouter | a local OpenAI-compatible server instead, e.g. `http://host.docker.internal:11434/v1` ([details](docs/local-models.md)) |
| `THICKET_MODEL_API_KEY` | | key for that server, if it wants one |
| `THICKET_MODEL` *chat* | `anthropic/claude-sonnet-5.5` | main chat model |
| `THICKET_FALLBACK_MODEL` *chat* | `google/gemini-3.8-flash` | used when the main model fails |
| `THICKET_CHEAP_MODEL` *chat* | `anthropic/claude-haiku-5.5` | memory review and personality checks |
| `THICKET_DAILY_BUDGET_USD` *chat* | `2` | daily spend cap (0 = none) |
| `THICKET_MONTHLY_BUDGET_USD` *chat* | `30` | monthly spend cap (0 = none) |
| `THICKET_SECRET_KEY` | generated | encryption key for stored secrets; if unset, `/data/secret.key` is created |
| `THICKET_PUBLIC_URL` | | public https URL, so avatar images in the characters folder can be served and webhook URLs are complete |
| `THICKET_IDLE_AFTER` | `20m` | quiet time before a conversation is reviewed for memories |
| `history_tokens` *chat only* | automatic | how much of the conversation each reply includes; older parts of long conversations are summarised |
| `THICKET_MESSAGE_RETENTION` | `720h` (30 days) | how long raw chat history is kept after review |
| `THICKET_DATA_DIR` | `/data` (Docker) | database, characters, backups |
| `THICKET_HTTP_ADDR` | `:8080` | `/health`, `/avatars/`, `/hooks/` and `/api/` |
| `THICKET_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Backups and moving house

```bash
docker compose exec thicket thicket export -o /data/backup.tar.gz
```

This writes the database and character files. Add `--include-key` to bring
the encryption key too; without it, stored API keys must be re-entered
after a restore. Restore into an empty data directory with
`thicket restore FILE` (`--force` to replace an existing one; the old files
are kept).

Thicket also keeps a daily database snapshot in `/data/backups` (last 7),
and snapshots before any schema migration. Memory reviews can be undone
one by one: `thicket memory runs`, then `thicket memory rollback RUN_ID`
(or ask Juni).

Share a sprig with `thicket character export pip` and add someone else's
with `thicket character import pip.md`. Memories never travel with a
character. Imported sprigs lose integration and admin access until you
grant it.

## Docs

- [Character file reference](docs/characters.md)
- [Integrations (MCP) setup](docs/mcp.md)
- [Webhooks](docs/webhooks.md)
- [Telegram](docs/telegram.md)
- [Local API and CLI](docs/api.md)
- [Skills](docs/skills.md)
- [Local models](docs/local-models.md)
- [How Thicket works](docs/architecture.md)
- [Changelog](CHANGELOG.md)

## Development

```bash
go test ./...
go run ./cmd/thicket check
```

Dependencies are deliberately few: `discordgo`, `modernc.org/sqlite` (pure
Go, no cgo), `yaml.v3` and `robfig/cron`. Adding one needs a reason.
