# Thicket

A small, self-hosted family of AI companions. Its characters are called
**sprigs**. They live in your Discord or Telegram, remember you, take a
little initiative, and stay true to who they are.

Talk to them in Discord, in the built-in web app, or both. The web app
installs on your phone and desktop and shows every sprig at once.

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

**Web app only?** Skip the chat apps: run `docker compose up -d`, then open
the sign-in link from `docker compose logs thicket` (or run
`docker compose exec thicket thicket web link`). Juni greets you in the
browser and asks for a model there. With Discord or Telegram:

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

Not sure something's right? `docker compose exec thicket thicket doctor`
checks the database, stored keys, the model server, chat app tokens and
each integration, and says what's wrong. `GET /health` reports each part
too. See [Running Thicket](docs/operations.md) for `doctor`, `costs`, the
"needs you" inbox and voice checks.

## The web app

Thicket serves a web app on the same port as `/health` (8080). Each sprig
has its own thread, there's an **Everyone** room where you address sprigs
by name, and you can open up to three chats side by side (Shift-click a
sprig, or use the split button on its row). The details panel shows what a
sprig remembers, its routines and how each last run went. It installs as
an app on phones and desktops (Add to Home Screen / Install), works in
light and dark, and can notify you when a sprig writes while it's in the
background.

Sign-in is by one-time link; there are no passwords:

- **From Discord:** DM Juni "send me a web app link". The link is posted by
  Thicket itself and never passes through the model.
- **From the server:** `docker compose exec thicket thicket web link`.
- **Fresh install without Discord:** the link is printed in the container
  logs on startup. Whoever opens it first becomes the owner.

Your web account *is* your Discord account when you sign in through a
link from Discord or as the owner, so sprigs remember the same things in
both places. Proactive messages (follow-ups, routine results, alerts) go
to where your account lives: Discord for Discord users, the sprig's thread
for web-only users.

To use it away from home, put it behind a reverse proxy with HTTPS and set
`THICKET_PUBLIC_URL` so links point at the right address. Sessions last 30
days from last use, are stored hashed, and use `HttpOnly`, `SameSite`
cookies (`Secure` over HTTPS).

## Talking to sprigs

- In DMs, just talk. The sprig you spoke to last keeps the conversation.
- Address someone by name to switch: `Pip, what's on tomorrow?`
- Sprigs work together: one can ask another a question mid-reply ("let me
  check with Pip"), hand you over, or give one a longer task to do in the
  background while you keep chatting; that sprig messages you when it's
  done.
- In server channels, sprigs answer when you @-mention the bot or start
  with a sprig's name. List channel ids in `THICKET_DISCORD_CHANNELS` to have
  them answer everything there. Telegram groups work the same way, with
  [one setting to change](docs/telegram.md#groups) for addressing by name.
- Ask Juni to bring someone on: "I need help keeping on top of my
  calendar". She interviews you (job, personality, voice, tools,
  schedule), the new sprig introduces itself as a preview, you tweak it,
  then she shows you every file it will get as a diff and nothing goes
  live until you approve. New sprigs start at `draft` and earn trust:
  they can ask for more, with their track record attached
  ([details](docs/characters.md#bringing-on-a-new-sprig)).
- Every sprig has a drawn, animated avatar that shows what it's doing
  (idle, thinking, working, sleeping, stuck).
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
- Routines can fire on events, not just the clock: "When I get an email
  from Maya, summarise it" watches an integration for new items
  ([details](docs/triggers.md)).
- Let other things wake a sprig: "Make a webhook that tells me when a CI
  build fails." Thicket DMs you a private URL; anything that POSTs to it
  starts the job with what it sent. See [Webhooks](docs/webhooks.md).
- See it all on one page: signed in to the web app, open `/dashboard` for
  your sprigs with live avatars, the "needs you" inbox (approve or deny
  there), the trail of what they did, and costs. See
  [Dashboard](docs/dashboard.md).
- Look things up: sprigs with web access search the web and read pages,
  through OpenRouter or your own SearXNG. See [Web search](docs/web.md).
- Teach them once: after working through a task, a sprig offers to save it
  as a skill, a versioned procedure every sprig follows from then on. Or
  start from the examples (`thicket skill starter`) or write your own. See
  [Skills](docs/skills.md).
- Script it: `thicket ask --sprig pip "what's on tomorrow?"` prints the
  reply, and `thicket run-job 4` runs a job now. Both use a local HTTP API
  with tokens from `thicket api-token create`. See [Local API](docs/api.md).
- Risky actions (sending email, controlling devices) show the exact action
  and wait for you to reply **approve** or **deny**. Each sprig's access to
  each tool can be set to `read`, `draft` (write it up, don't do it), `ask`
  or `act` ([details](docs/mcp.md#approvals-and-permission-levels)).
- Nothing fails silently: failed or interrupted jobs and tasks, and
  results that couldn't be delivered, land in a "needs you" inbox. Ask
  Juni "what needs me?" or run `thicket inbox`.
- Changed your mind? Say **stop** (or "never mind", 🛑, `/stop`) while a
  sprig is working: it stops at once, tells you anything it had already
  done, and sets aside messages you sent in the meantime. With no reply
  running, "stop" stops your running jobs.
- Pin what matters: reply to a message with **pin this** (or say **pin
  that** for the latest one) and it stays in every reply's context for the
  rest of the conversation, even after older messages are summarised (up
  to 10 pins). **What's pinned?**, **unpin this** and **unpin all** do what
  they say. Pins end with the conversation; for anything longer, ask a
  sprig to remember it.
- Take things back: reply to a message with **forget this** and it's gone
  from the conversation sprigs see (a sprig's message is deleted from the
  chat too). **Delete that** deletes the sprig's last message. Deleting
  your own message in Discord or the web app does the same; Telegram
  doesn't tell bots about deletions, so reply "forget this" there. Plain
  "forget that" is still about memories. The web app has pin and delete
  buttons on every message.

## Configuration

Everything has a sane default. Settings marked *chat* can also be changed
by asking Juni (as the owner), and the chat value wins.

| Variable | Default | |
|---|---|---|
| `DISCORD_TOKEN` | | Discord bot token; without it (and `TELEGRAM_TOKEN`) Thicket runs as a web app only |
| `TELEGRAM_TOKEN` | | Telegram bot token ([details](docs/telegram.md)) |
| `THICKET_OWNER_DISCORD_ID` | first DM | your Discord user id |
| `THICKET_OWNER_TELEGRAM_ID` | first DM | your numeric Telegram user id |
| `THICKET_ALLOWED_USERS` | | comma-separated user ids allowed in, plain or as `telegram:ID` / `discord:ID` (or ask Juni to invite someone) |
| `THICKET_DISCORD_CHANNELS` | | Discord channel ids where sprigs answer every message |
| `THICKET_COMMONS_CHANNEL` | | a channel where sprigs hand work to each other by @mention ([details](docs/triggers.md#the-commons)); a Discord id, or `telegram:ID` |
| `THICKET_TELEGRAM_CHATS` | | Telegram group ids where sprigs answer every message |
| `OPENROUTER_API_KEY` | | optional; Juni can collect it in a DM |
| `THICKET_MODEL_URL` *chat* | OpenRouter | a local OpenAI-compatible server instead, e.g. `http://host.docker.internal:11434/v1` ([details](docs/local-models.md)) |
| `THICKET_MODEL_API_KEY` | | key for that server, if it wants one |
| `THICKET_SEARCH_URL` *chat* | | a SearXNG instance for web search ([details](docs/web.md)) |
| `THICKET_WEB_PROXY` | `0` | `1` reads web pages through `HTTPS_PROXY` ([trade-off](docs/web.md#behind-a-proxy)) |
| `THICKET_MODEL` *chat* | `anthropic/claude-sonnet-5.5` | main chat model |
| `THICKET_FALLBACK_MODEL` *chat* | `google/gemini-3.8-flash` | used when the main model fails |
| `THICKET_CHEAP_MODEL` *chat* | `anthropic/claude-haiku-5.5` | memory review and personality checks |
| `THICKET_DAILY_BUDGET_USD` *chat* | `2` | daily spend cap (0 = none) |
| `THICKET_MONTHLY_BUDGET_USD` *chat* | `30` | monthly spend cap (0 = none) |
| `THICKET_SECRET_KEY` | generated | encryption key for stored secrets; if unset, `/data/secret.key` is created |
| `THICKET_PUBLIC_URL` | | public https URL, used in web app sign-in links, webhook URLs and for avatar images |
| `THICKET_IDLE_AFTER` | `20m` | quiet time before a conversation is reviewed for memories |
| `history_tokens` *chat only* | automatic | how much of the conversation each reply includes; older parts of long conversations are summarised |
| `THICKET_MESSAGE_RETENTION` | `720h` (30 days) | how long raw chat history is kept after review |
| `THICKET_DATA_DIR` | `/data` (Docker) | database, characters, backups |
| `THICKET_HTTP_ADDR` | `:8080` | the web app, `/dashboard`, `/health`, `/avatars/`, `/hooks/` and `/api/v1/` |
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
with `thicket character import pip.md`. Memories never travel with an
exported sprig. Imported sprigs lose integration and admin access until you
grant it.

Coming from another app? `thicket character import` also reads character
cards (`.json` or `.png`), an OpenClaw workspace, a Hermes Agent home, and
a Grok persona or any system prompt saved as a text file, and can bring
their memories of you along. You see the sprig before anything is written:
see [Bringing a character from another app](docs/characters.md#bringing-a-character-from-another-app).

## Docs

- [Character file reference](docs/characters.md)
- [Integrations (MCP) setup](docs/mcp.md)
- [Webhooks](docs/webhooks.md)
- [Event triggers and the commons](docs/triggers.md)
- [Telegram](docs/telegram.md)
- [Local API and CLI](docs/api.md)
- [Skills](docs/skills.md)
- [Local models](docs/local-models.md)
- [Web search](docs/web.md)
- [Dashboard](docs/dashboard.md)
- [Running Thicket: doctor, costs, inbox, voice checks](docs/operations.md)
- [How Thicket works](docs/architecture.md)
- [Changelog](CHANGELOG.md)

## Development

```bash
go test ./...
go run ./cmd/thicket check
```

The web app is plain HTML, CSS and JavaScript in `internal/web/static`,
embedded in the binary. There is no build step: edit and rebuild.

Dependencies are deliberately few: `discordgo`, `modernc.org/sqlite` (pure
Go, no cgo), `yaml.v3` and `robfig/cron`. Adding one needs a reason.
