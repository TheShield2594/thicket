# Thicket

A small, self-hosted family of AI companions. Its characters are called
**sprigs**. They live in your Discord, remember you, take a little
initiative, and stay true to who they are.

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

**Web app only?** Skip Discord: run `docker compose up -d`, then open the
sign-in link from `docker compose logs thicket` (or run
`docker compose exec thicket thicket web link`). Juni greets you in the
browser and asks for an OpenRouter key there. With Discord:

1. **Create a Discord bot.** At <https://discord.com/developers/applications>,
   make an application, add a bot, and switch on **Message Content Intent**.
   Copy the bot token. Invite it to your server with the `bot` scope and
   these permissions: Send Messages, Read Message History, **Manage Webhooks**
   (so each sprig posts with its own name and avatar).
2. **Run it.**

   ```bash
   DISCORD_TOKEN=your-bot-token docker compose up -d
   ```

   In Portainer: Stacks → Add stack → paste `docker-compose.yml` → add
   `DISCORD_TOKEN` as an environment variable → Deploy. This pulls
   `ghcr.io/theshield2594/thicket:latest`, which CI publishes on every push
   to `main`. (If the package is private, make it public in the repo's
   Packages settings or log Portainer in to ghcr.io.) The optional
   variables listed in `docker-compose.yml` can be added the same way.
   `THICKET_DATA_DIR` and `THICKET_HTTP_ADDR` are fixed inside the image
   (`/data` and `:8080`): change the volume or port mapping in the compose
   file instead.
3. **DM the bot.** Juni says hello and asks for an
   [OpenRouter](https://openrouter.ai/keys) key. Paste it in the DM: she
   checks it works and stores it encrypted. Then she'll learn your name,
   your timezone, and what you'd like help with.

The first person to DM the bot becomes the owner, unless you set
`THICKET_OWNER_DISCORD_ID`.

Not sure something's right? `docker compose run --rm thicket check` says
exactly what's missing, and `GET /health` reports each part.

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
- In server channels, sprigs answer when you @-mention the bot or start
  with a sprig's name. List channel ids in `THICKET_DISCORD_CHANNELS` to have
  them answer everything there.
- Ask Juni to bring someone on: "I need help keeping on top of my
  calendar". She interviews you, the new sprig introduces itself as a
  preview, you tweak it, and the character file is written for you.
- Schedule things: "Every weekday at 7:30, give me a summary of my day."
- Risky actions (sending email, controlling devices) show the exact action
  and wait for you to reply **approve** or **deny**.

## Configuration

Everything has a sane default. Settings marked *chat* can also be changed
by asking Juni (as the owner), and the chat value wins.

| Variable | Default | |
|---|---|---|
| `DISCORD_TOKEN` | | Discord bot token; without it Thicket runs as a web app only |
| `THICKET_OWNER_DISCORD_ID` | first DM | your Discord user id |
| `THICKET_ALLOWED_USERS` | | comma-separated Discord ids allowed in (or ask Juni to invite someone) |
| `THICKET_DISCORD_CHANNELS` | | channel ids where sprigs answer every message |
| `OPENROUTER_API_KEY` | | optional; Juni can collect it in a DM |
| `THICKET_MODEL` *chat* | `anthropic/claude-sonnet-5.5` | main chat model |
| `THICKET_FALLBACK_MODEL` *chat* | `google/gemini-3.8-flash` | used when the main model fails |
| `THICKET_CHEAP_MODEL` *chat* | `anthropic/claude-haiku-5.5` | memory review and personality checks |
| `THICKET_DAILY_BUDGET_USD` *chat* | `2` | daily spend cap (0 = none) |
| `THICKET_MONTHLY_BUDGET_USD` *chat* | `30` | monthly spend cap (0 = none) |
| `THICKET_SECRET_KEY` | generated | encryption key for stored secrets; if unset, `/data/secret.key` is created |
| `THICKET_PUBLIC_URL` | | public https URL, used in web app sign-in links and for avatar images |
| `THICKET_IDLE_AFTER` | `20m` | quiet time before a conversation is reviewed for memories |
| `THICKET_MESSAGE_RETENTION` | `720h` (30 days) | how long raw chat history is kept after review |
| `THICKET_DATA_DIR` | `/data` (Docker) | database, characters, backups |
| `THICKET_HTTP_ADDR` | `:8080` | the web app, `/health` and `/avatars/` |
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
