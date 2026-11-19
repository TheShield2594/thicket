# Thicket

A small, self-hosted, open-source family of AI companions, for one person
or a whole household: an open-source alternative to Grok Bot that keeps
your data on your own server. Its characters are called **sprigs**. They
live in your Discord or Telegram, remember you, take a little initiative,
and stay true to who they are.

Each person has private sprigs of their own, the household shares family
sprigs, and kids get kid sprigs that are locked down in code
([Households](docs/household.md)). The core stays small: integrations are
adapters you connect.

Talk to them in Discord, in the built-in web app, or both. The web app
installs on your phone and desktop and shows every sprig at once, and
there's a [desktop app](docs/desktop.md) for macOS, Windows and Linux with
a tray icon and native notifications.

Thicket is one Go binary and one SQLite file, plus an optional sandbox
container that sprigs use as their computer. No vector database, no
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

About five minutes, on any machine with Docker: a PC, a NAS, or a
Raspberry Pi 4 or 5 (the images are built for amd64 and arm64).

1. **Get the compose file** into a new folder.

   ```bash
   curl -fsSLO https://raw.githubusercontent.com/TheShield2594/thicket/main/docker-compose.yml
   ```

2. **Set it up.** `thicket init` asks a few questions, checks each answer
   against the real service, and stores keys and tokens encrypted. Every
   question can be skipped with Enter and answered later.

   ```bash
   docker compose run --rm thicket init
   ```

   - **The model:** an [OpenRouter](https://openrouter.ai/keys) key, or
     the address of your own Ollama or LM Studio server
     ([Local models](docs/local-models.md)).
   - **Discord and Telegram tokens** (both optional; without them you use
     the web app). On Discord: at the
     [Developer Portal](https://discord.com/developers/applications)
     create an application, add a bot, switch on **Message Content
     Intent**, copy the token, and invite it with the `bot` scope and
     Send Messages, Read Message History and **Manage Webhooks** (so each
     sprig posts with its own name and avatar). On Telegram: message
     [@BotFather](https://t.me/BotFather), send `/newbot`, copy the
     token ([Telegram](docs/telegram.md)).
   - **Timezone, spend caps, and who the owner is.**

3. **Start it.**

   ```bash
   docker compose up -d
   ```

4. **Say hello.** Open the sign-in link `init` printed (the web app, at
   `http://localhost:8080`), or DM the bot (on Telegram, open it and press
   Start). Juni introduces herself and takes it from there.

Prefer not to use a terminal? Skip step 2: paste `docker-compose.yml` into
Portainer (Stacks → Add stack), deploy, and open the sign-in link from the
container's log. Juni asks for the model key in chat instead, and connects
Discord or Telegram there too: tell her "connect Discord" and send her the
bot token. She connects it on the spot, with no restart, and gives you
the link that adds the bot to your server. Tokens can
also be given as `DISCORD_TOKEN` / `TELEGRAM_TOKEN` environment variables;
they are stored encrypted on first start, and `thicket init --from-env`
checks them first. The other variables in `docker-compose.yml` work the
same way. `THICKET_DATA_DIR` and `THICKET_HTTP_ADDR` are fixed inside the
image (`/data` and `:8080`): change the volume or port mapping instead.

The compose file runs the newest release (`latest`). To stay on one
version set `THICKET_VERSION=0.2.0` in a `.env` file beside it; `edge` is
whatever is on `main`. If the image can't be pulled, the package may still
be private: make it public in the repository's Packages settings.

### Updating

```bash
docker compose pull
docker compose stop thicket
docker compose run --rm thicket update
docker compose up -d
```

`thicket update` shows what changed, takes a full backup, migrates the
database and checks it. If the check fails it puts everything back exactly
as it was and tells you to stay on the old version
([Running Thicket](docs/operations.md#thicket-update)).

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
sprig, or use the split button on its row). Attach files with the paperclip (or
drop them on the message box), or tap the microphone to record a voice
message: tap again to send it, and the sprig answers what you said. The details panel shows what a
sprig remembers, its routines and how each last run went. **Memory** in
the sidebar shows what sprigs remember about you, relationship memory
(how you like each sprig to talk to you, what landed, what annoyed you)
apart from facts, with where each came from; edit, lock, undo or delete
any of it, and the next reply follows. Parents see a kid sprig's there
too, on its chats page. It installs as
an app on phones and desktops (Add to Home Screen / Install), works in
light and dark, and can notify you when a sprig writes while it's in the
background.

### Signing in

Three ways, all ending in the same 30-day session (90 days at most):

- **Username and password.** The owner adds people with
  `thicket user add kim` (or `--user ID` to give someone already on
  Discord or Telegram a login), or makes an invite link on the dashboard
  or with `thicket user invite`: the person opens it and chooses a
  username and password. Anyone signed in can set or change their own
  password with the key button by their name; changing it signs out
  their other sessions. Passwords are hashed with argon2id; five wrong
  ones lock that username for 15 minutes.
- **Single sign-on (optional).** Any OpenID Connect provider: Authentik,
  Authelia, Keycloak, Google. Set `THICKET_OIDC_ISSUER`,
  `THICKET_OIDC_CLIENT_ID` and `THICKET_OIDC_CLIENT_SECRET` (stored
  encrypted on start, like the chat tokens) and `THICKET_PUBLIC_URL`, and
  register `<public url>/auth/oidc/callback` as the redirect URI. People
  are matched by their account at the provider, else by a verified email
  you've given them (`thicket user email ID ADDRESS`), else by the invite
  they came with. Set `THICKET_OIDC_MEMBER_GROUP` and/or
  `THICKET_OIDC_ADMIN_GROUP` to let anyone in those groups in (their role
  follows the group; the owner's never changes) and keep everyone else
  out.
- **One-time link**, as before:
  - **From Discord:** DM Juni "send me a web app link". The link is posted by
    Thicket itself and never passes through the model.
  - **From the server:** `docker compose exec thicket thicket web link`.
  - **Fresh install without Discord:** the link is printed in the container
    logs on startup. Whoever opens it first becomes the owner.

Each person sees the shared sprigs and their own: a sprig with
`belongs_to` set ([details](docs/characters.md#fields)) is invisible to
everyone else, in the web app, the dashboard and chat. See and sign out
your sessions under the key button or on the dashboard.

Your web account *is* your Discord account when you sign in through a
link from Discord or as the owner, so sprigs remember the same things in
both places. Proactive messages (follow-ups, routine results, alerts) go
to the chat app you last wrote from, preferring Telegram or Discord over
the web app when you have both; say "reach me on Telegram" to choose (for
every sprig, or one). If that app fails, your other linked apps are
tried. Web-only users get them in the sprig's thread (with a browser
notification if turned on), and Juni mentions once that a chat app would
bring them to their phone.

To use it away from home, put it behind a reverse proxy with HTTPS and set
`THICKET_PUBLIC_URL` so links point at the right address. Sessions last 30
days from last use, are stored hashed, and use `HttpOnly`, `SameSite`
cookies (`Secure` over HTTPS).

## Talking to sprigs

- In DMs, just talk. The sprig you spoke to last keeps the conversation.
- Address someone by name to switch: `Pip, what's on tomorrow?`
- Or let Thicket pick: ask Juni to turn on `auto_route` and a message
  that names nobody goes to the sprig whose role fits it ("what's on
  tomorrow?" reaches Pip). The sprig already talking keeps the
  conversation unless the message is plainly for someone else, and any
  doubt or failure leaves it where it was. It costs one small model call
  per un-named message.
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
  (idle, thinking, working, sleeping, stuck) and how its last reply went
  (pleased, unsure).
- Send voice messages (in Discord, Telegram or the web app): they're transcribed (by the `transcribe_model`
  setting, a model that hears audio) and the sprig answers what you said.
  Needs OpenRouter.
- Send pictures, PDFs and text files (from the web app too). Sprigs look at images (the chat
  model needs vision, as the defaults have), read PDFs (up to 2 per
  message, 10 MB and 50 pages each; 2 MB if its pages can't be counted) and text files such as notes, CSVs or
  logs. Images are seen for that reply only. A PDF's text (its first
  20,000 characters, the `document_chars` setting) is kept for follow-up questions until the
  conversation goes quiet; text files stay in the conversation like any
  message. Other files (Word documents, for example) are passed on by
  name only.
- PDFs are read by the model itself when it can, otherwise by OpenRouter's
  OCR ($2 per 1,000 pages). Ask Juni to set `pdf_engine` to
  `cloudflare-ai` (free, text only), `mistral-ocr` (best for scans) or
  `native`; the default is `auto`.
- Share the house: family sprigs everyone uses, with family memory kept
  apart from each person's own; solo sprigs only you see; kid sprigs that
  only chat, only use tools a parent approved, and alert parents if
  something worrying comes up. Every permission change is in the trail
  and can be undone. See [Households](docs/household.md).
- One calendar for the household: connect your calendars (ICS or CalDAV,
  read-only), and see your sprigs' jobs and what they put on the calendar,
  with toggles per layer and per sprig, in the web app or an app. See
  [Calendar](docs/calendar.md).
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
- Give a sprig a computer: the bundled sandbox container is a small
  Linux machine where sprigs run commands and keep files ("clone this
  repo and run its tests"). It can't see Thicket's data, and commands ask
  for your OK unless you say otherwise. See [The sandbox](docs/sandbox.md).
- Look things up: sprigs with web access search the web and read pages,
  through OpenRouter or your own SearXNG. See [Web search](docs/web.md).
- Teach them once: after working through a task, a sprig offers to save it
  as a skill, a versioned procedure every sprig follows from then on. Or
  start from the examples (`thicket skill starter`) or write your own. See
  [Skills](docs/skills.md).
- Script it: `thicket ask --sprig pip "what's on tomorrow?"` prints the
  reply, and `thicket run-job 4` runs a job now. Both use a local HTTP API
  with tokens from `thicket api-token create`. See [Local API](docs/api.md).
- Let your other AI tools in: `thicket mcp` is an MCP server, so Claude
  Desktop, Claude Code or Cursor can ask your sprigs things and run jobs.
  They can't approve risky actions: that stays with you. See
  [Thicket as an MCP server](docs/mcp-server.md).
- Connect services you sign in to: "Connect Notion at
  https://mcp.notion.com/mcp" gets you a sign-in link, and the tokens are
  stored encrypted and refreshed for you. See
  [Integrations](docs/mcp.md#services-you-sign-in-to-oauth).
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
- Pull the plug: `thicket pause` stops everything within a second, and
  nothing runs (replies, jobs, triggers, check-ins, model calls) until
  `thicket resume`, across restarts. Pause one sprig with `--sprig`, or
  from the dashboard or the API. Sprigs' tool use is rate-limited per
  sprig and per integration
  ([details](docs/operations.md#pausing-the-kill-switch)).
- Each sprig keeps to its own files and can't run commands until its
  owner says so ([Sandboxing](docs/mcp.md#sandboxing-files-and-commands)).
- Pin what matters: reply to a message with **pin this** (or say **pin
  that** for the latest one) and it stays in every reply's context for the
  rest of the conversation, even after older messages are summarised (up
  to 10 pins). **What's pinned?**, **unpin this** and **unpin all** do what
  they say. Pins end with the conversation; for anything longer, ask a
  sprig to remember it.
- Remember what matters for good: reply to any message with **remember
  this** (or say **remember this: …**), react with 🧠 or 📌 on Discord or
  ✍ on Telegram, or tap the brain button in the web app. It becomes a
  locked memory you set, without the sprig having to decide; reply
  **forget this** to the sprig's confirmation to undo it. In a family
  sprig's chat you're asked once whether these are just yours or the
  family's; kids' always stay their own.
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
| `DISCORD_TOKEN` | | Discord bot token; without it (and `TELEGRAM_TOKEN`) Thicket runs as a web app only. Stored encrypted on first start, so you can then remove it ([details](docs/operations.md#secrets)). Optional: an admin can send the token to Juni in a DM instead, which connects Discord while Thicket runs |
| `TELEGRAM_TOKEN` | | Telegram bot token ([details](docs/telegram.md)); stored like `DISCORD_TOKEN` |
| `THICKET_OWNER_DISCORD_ID` | first DM | your Discord user id |
| `THICKET_OWNER_TELEGRAM_ID` | first DM | your numeric Telegram user id |
| `THICKET_ALLOWED_USERS` | | comma-separated user ids allowed in, plain or as `telegram:ID` / `discord:ID` (or ask Juni to invite someone) |
| `THICKET_DISCORD_CHANNELS` | | Discord channel ids where sprigs answer every message |
| `THICKET_COMMONS_CHANNEL` | | a channel where sprigs hand work to each other by @mention ([details](docs/triggers.md#the-commons)); a Discord id, or `telegram:ID` |
| `THICKET_TELEGRAM_CHATS` | | Telegram group ids where sprigs answer every message |
| `OPENROUTER_API_KEY` | | optional; Juni can collect it in a DM |
| `THICKET_MODEL_URL` *chat* | OpenRouter | a local OpenAI-compatible server instead, e.g. `http://host.docker.internal:11434/v1` ([details](docs/local-models.md)) |
| `THICKET_MODEL_API_KEY` | | key for that server, if it wants one |
| `THICKET_SANDBOX_URL` | `http://sandbox:7070` in compose | the sandbox container ([details](docs/sandbox.md)); empty turns it off |
| `THICKET_SEARCH_URL` *chat* | | a SearXNG instance for web search ([details](docs/web.md)) |
| `THICKET_WEB_PROXY` | `0` | `1` reads web pages through `HTTPS_PROXY` ([trade-off](docs/web.md#behind-a-proxy)) |
| `THICKET_MODEL` *chat* | `anthropic/claude-sonnet-5.5` | main chat model |
| `THICKET_FALLBACK_MODEL` *chat* | `google/gemini-3.8-flash` | used when the main model fails |
| `THICKET_CHEAP_MODEL` *chat* | `anthropic/claude-haiku-5.5` | memory review and personality checks |
| `THICKET_DAILY_BUDGET_USD` *chat* | `2` | daily spend cap (0 = none) |
| `THICKET_MONTHLY_BUDGET_USD` *chat* | `30` | monthly spend cap (0 = none) |
| `THICKET_SECRET_KEY` | generated | encryption key for stored secrets; if unset, `/data/secret.key` is created |
| `THICKET_OIDC_ISSUER`, `THICKET_OIDC_CLIENT_ID`, `THICKET_OIDC_CLIENT_SECRET` | | single sign-on ([details](#signing-in)); stored encrypted on first start |
| `THICKET_OIDC_NAME` | `single sign-on` | the button's label, e.g. `Authentik` |
| `THICKET_OIDC_MEMBER_GROUP`, `THICKET_OIDC_ADMIN_GROUP` | | groups (in `THICKET_OIDC_GROUPS_CLAIM`, default `groups`) whose people may sign in, as members or admins |
| `THICKET_OIDC_SCOPES` | `openid email profile` (+`groups` with a group set) | scopes to ask for |
| `THICKET_SECRET_KEY_FILE` | | a file holding that key instead, e.g. a Docker secret at `/run/secrets/thicket_secret_key` |
| `THICKET_PUBLIC_URL` | | public https URL, used in web app sign-in links, webhook URLs, integration sign-in (OAuth) callbacks and for avatar images |
| `transcribe_model` *chat only* | `google/gemini-3.8-flash` | model that transcribes voice messages; it must accept audio |
| `auto_route` *chat only* | `false` | when a message names no sprig, the cheap model picks who answers by role |
| `THICKET_IDLE_AFTER` | `20m` | quiet time before a conversation is reviewed for memories |
| `history_tokens` *chat only* | automatic | how much of the conversation each reply includes; older parts of long conversations are summarised |
| `document_chars` *chat only* | `20000` | how much of a PDF's text is kept for follow-up questions (0 = none); it's re-sent with every later reply |
| `THICKET_MESSAGE_RETENTION` | `720h` (30 days) | how long raw chat history is kept after review |
| `THICKET_DATA_DIR` | `/data` (Docker) | database, characters, backups |
| `THICKET_HTTP_ADDR` | `:8080` | the web app, `/dashboard`, `/health`, `/avatars/`, `/hooks/` and `/api/v1/` |
| `THICKET_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Backups and moving house

```bash
docker compose exec thicket thicket backup -o /data/backup.tar.gz
```

This writes one archive with the database (a consistent copy, safe to take
while Thicket runs) and every sprig's files: character file, avatar
settings and voice tests. Add `--include-key` to bring the encryption key
too; without it, stored API keys must be re-entered after a restore, and
with it the file is as sensitive as a password.

Restore into an empty data directory with `thicket restore FILE`. The
archive is checked first (its manifest, that its database isn't from a
newer Thicket) and nothing is touched if it doesn't pass. Stop Thicket
before restoring over an existing install, then pass `--force`; the old
files are kept beside the new ones.

For backups on a schedule, give Thicket a folder (ask Juni, or
`thicket settings set backup_dir /backups`). In Docker it has to be a
mounted folder, such as `- /mnt/nas/thicket:/backups` under `volumes:`.
`backup_schedule` is a cron line (default `0 3 * * *`, 03:00 daily) and
`backup_keep` is how many to keep (default 7). A backup that fails goes to
your needs-you list and Juni tells you; the dashboard's Health section
shows when the last one ran and how big it was.

Thicket also keeps a daily database snapshot in `/data/backups` (last 7),
and snapshots before any schema migration. Memory reviews can be undone
one by one: `thicket memory runs`, then `thicket memory rollback RUN_ID`
(or ask Juni).

Share a sprig with `thicket export-sprig pip`: one `pip.sprig` file with
its character, routines, the skills it uses, avatar and voice tests. Add
someone else's with `thicket import-sprig pip.sprig`, or send the file to
Juni in chat. Secrets and chats never travel with a sprig, and memories
only when you add `--memories` (for moving a sprig to a new Thicket);
imported sprigs start at draft and lose integration and admin access until
you grant it. See [Sharing a sprig](docs/characters.md#sharing-a-sprig).

Coming from another app? `thicket character import` also reads character
cards (`.json` or `.png`), an OpenClaw workspace, a Hermes Agent home, and
a Grok persona or any system prompt saved as a text file, and can bring
their memories of you along. You see the sprig before anything is written:
see [Bringing a character from another app](docs/characters.md#bringing-a-character-from-another-app).

## Docs

- [Writing a sprig](docs/authoring.md): personality, voice, tools,
  routines, looks, voice tests, sharing
- [Character file reference](docs/characters.md)
- [Households: solo, family and kid sprigs](docs/household.md)
- [Calendar](docs/calendar.md)
- [Integrations (MCP) setup](docs/mcp.md)
- [The sandbox: sprigs' own computer](docs/sandbox.md)
- [Webhooks](docs/webhooks.md)
- [Event triggers and the commons](docs/triggers.md)
- [Telegram](docs/telegram.md)
- [Local API and CLI](docs/api.md)
- [API v2 for apps](docs/api-v2.md)
- [Thicket as an MCP server](docs/mcp-server.md)
- [Skills](docs/skills.md)
- [Local models](docs/local-models.md)
- [Web search](docs/web.md)
- [Dashboard](docs/dashboard.md)
- [Desktop app](docs/desktop.md)
- [Running Thicket: doctor, costs, inbox, voice checks](docs/operations.md)
- [How Thicket works](docs/architecture.md)
- [Extending Thicket: chat adapters and integrations](docs/adapters.md)
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
