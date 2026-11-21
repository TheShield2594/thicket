# Character file reference

Each sprig is one Markdown file in the characters directory
(`/data/characters` in Docker), named after its id: `pip.md` has `id: pip`.
The file starts with YAML frontmatter between `---` lines. Everything after
the second `---` is free-form background that goes into the sprig's prompt
as-is: backstory, appearance, example lines.

You rarely need to write one by hand: ask Juni to bring someone on and she
writes it for you. But you can always edit the files, and **your edits
win**: Thicket remembers the hash of every file it writes, and if you've
changed one by hand it never overwrites it. It saves its version as
`<id>.proposed.md` next to yours instead, and tells you.

Changes are picked up within about 10 seconds, no restart needed. If a file
has a problem, it's skipped and the host sprig tells you exactly what's
wrong.

## Example

```markdown
---
id: pip
name: Pip
role: Keeps the calendar and makes sure nothing sneaks up on you.
avatar: pip.png
color: "#e0a458"
personality:
  traits:
    - punctual
    - upbeat
    - a bit of a worrier
  quirks:
    - counts down to everything
  catchphrases:
    - "Tick tock!"
voice:
  style:
    - Brisk, cheerful, short sentences.
    - Always gives times in the user's timezone.
  never_say:
    - "I'm just a bot"
relationships:
  user: Their trusty timekeeper. Proud when they're early.
  characters:
    juniper: Looks up to Juni, who brought Pip on.
tools:
  - schedule
  - mcp:calendar/*
initiative:
  check_ins: false
  follow_ups: true
  quiet_hours: "22:00-08:00"
  daily_messages: 3
---

Pip is a small, round sprig with a pocket watch for a belly...
```

## Fields

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | 2-32 lowercase letters, digits, dashes. Must match the file name. |
| `name` | yes | Full name. |
| `nickname` | | What they go by in chat (Juniper → Juni). Users can address either. |
| `role` | yes | One sentence: who they are and what they're for. |
| `host` | | `true` for exactly one sprig: the one that greets new users, runs onboarding and reports problems. Juniper by default. |
| `avatar` | | An `https://` image URL, or a file name in the characters folder (served at `/avatars/` when `THICKET_PUBLIC_URL` is set). |
| `color` | | Hex colour for the side bar of DM messages, e.g. `"#5aa469"`. |
| `personality.traits` | yes | 1-6 core traits; 3-4 works best. |
| `personality.quirks` | | Habits and oddities. |
| `personality.catchphrases` | | Used sparingly, not in every message. |
| `voice.style` | yes | Rules for how they talk. |
| `voice.never_say` | | Phrases they never say. Checked in code before every reply; a reply that uses one is rewritten. |
| `voice.speaks` | | When they answer out loud: `never`, `when_spoken_to` (the default: a voice message gets a voice message back) or `always`. See [Speaking out loud](#speaking-out-loud). |
| `voice.sounds_like` | | The speech voice they speak in, as the speech server names it (`nova`, `af_heart`). Empty: the `speech_voice` setting. |
| `relationships.user` | | How they see you. |
| `relationships.characters` | | How they see other sprigs, keyed by id. |
| `tools` | | Tool scopes, below. |
| `initiative.check_ins` | | May send a gentle check-in after you've been quiet for a few days (`checkin_days`, default 3). At most one per quiet spell, across all sprigs. |
| `initiative.follow_ups` | | Follows up on things you mentioned ("how did the interview go?"). |
| `initiative.quiet_hours` | | `"22:00-08:00"` in your timezone: no proactive messages then. Job and trigger results that finish during quiet hours are held and delivered when they end. |
| `initiative.daily_messages` | | How many messages the sprig may start itself (job and trigger results, check-ins, follow-ups) per person per day, in that person's timezone: 1-50, default 3. Once it's used up, results wait for the next day. Replies never count. Ask the sprig ("message me at most 5 times a day") to change it, or its quiet hours, in chat. |
| `initiative.morning_brief` | | A time like `"07:30"` (each person's timezone): a short brief of today's calendar and anything due. Off when left out. See [Briefs and reminders](#briefs-and-reminders). |
| `initiative.event_reminders` | | Minutes before each calendar event for a one-line heads-up (10-1440; reminders are checked every five minutes). Off when left out. |
| `initiative.due_reminders` | | `true`: a reminder when something noted as due ("the bill is due Friday") is coming up. |
| `actions.quiet` | | `true` keeps routine built-in work (scheduling jobs, making webhooks and triggers) to the trail, without a line in the chat. What it does through integrations is always said, with undo where there is one. See [Saying what was done](mcp.md#saying-what-was-done-and-undo). |
| `model` | | Override the chat model for this sprig (an OpenRouter model id). |
| `created_by` | | Informational. |
| `belongs_to` | | A Thicket user id (`thicket user list`): the sprig is that person's own, and only they can talk to it, see it in the web app or its dashboard card, or hand work to it. Leave it off (the default) for a sprig everyone shares. The host is always shared. Ask Juni for a "private" sprig and she sets it to you. |
| `household` | | The sprig's tier: `solo` (one person's: needs `belongs_to`), `family` (shared by every adult; the default without `belongs_to`) or `kid`. See [Households](household.md). |
| `kids` | kid sprigs | User ids of the kids it talks to. |
| `parents` | kid sprigs | User ids of the adults who set it up: they read its chats, get its safety alerts and choose what it may use. |
| `allow` | | Kid sprigs only: adult data a parent lets it see, `family_memory` and/or `calendar`, and `voice` to let it answer out loud. Nothing by default. |

Unknown fields are an error, so typos don't silently do nothing.

### Speaking out loud

Send a sprig a voice message (a Telegram voice note, a Discord audio
file, or the web app's recorder) and, by default, it answers with one:
the text reply first, then the same reply read aloud in its voice.
Telegram gets a voice note, Discord an audio attachment, and the web app
a player under the reply. `voice.speaks: always` has it answer out loud
every time; `never` keeps it to text. Scheduled jobs, check-ins and
notices are never spoken.

```yaml
voice:
  style:
    - Brisk, cheerful, short sentences.
  speaks: when_spoken_to
  sounds_like: nova
```

Each person chooses how each sprig talks back to them: **full** reads the
whole reply (the default), **brief** says a short version and sends the
full text alongside, **off** is text only. Say "voice brief", "voice
full" or "voice off" to the sprig (or "voice" to hear how it's set), or
use the Voice buttons in its details in the web app. Code, links and
formatting aren't read out, and very long replies are cut at about 4,000
characters with "the rest is in the message".

Speech is made by an OpenAI-compatible speech endpoint
(`POST /v1/audio/speech`): the [voice plugin](voice.md) (Piper) when it's
running and `local_voice` is on, otherwise the model server (`model_url`,
OpenRouter), or a local speech server such as Kokoro-FastAPI beside
Ollama, set with `speech_url`. `speech_model` and `speech_voice` choose
the model and the default voice. Spoken replies count against the spend
caps like any model call: OpenRouter speech is priced at
`speech_usd_per_1k_chars` (default $0.015 per 1,000 characters), and a
local speech server is free. If speech fails, the text has already gone
out, and admins are told once a day.

Kid sprigs speak only once a parent allows it (`allow: [voice]`, or the
Voice box on the web app's kid screen).

### Quiet hours and the daily budget

Quiet hours and `daily_messages` apply to every message a sprig starts
itself. A held result is delivered when quiet hours end, or with the next
day's budget, with a note saying when it was written; one still held after
a day goes to **Needs you** on the dashboard instead, and the trail says
so. Check-ins and follow-ups simply wait for their next chance. Some
things always get through at once and don't count: replies to you,
requests for your approval, safety alerts to parents, and failures that
need you. If one arrives during your quiet hours (or past the day's
limit), it says why. The web app's sprig details and the dashboard show
how much of today's budget is used and what's held.

### Briefs and reminders

A sprig can speak first, without a scheduled job:

- **Morning brief** (`initiative.morning_brief: "07:30"`): at that time in
  your timezone, today's calendar (your calendars, shared ones and what
  sprigs put on it) and anything due today, in a few lines.
- **Event heads-ups** (`initiative.event_reminders: 30`): "you have the
  dentist at 3", that many minutes before each timed event on your
  calendar. Once per event, whichever sprig gets there first.
- **Due reminders** (`initiative.due_reminders: true`): when something a
  sprig noted with a due time (a follow-up: "the water bill is due Friday
  at 5") is coming up, within the next three hours. Once per item. A
  sprig only mentions its own follow-ups and shared ones.

Switch them on in the file or in chat ("Pip, send me a morning brief at
7:30", "remind me half an hour before things"; Juni offers them when she
sets up a new sprig). What to say is gathered by Thicket; the sprig writes
it in its own voice, and it goes the way every unprompted message goes:
quiet hours and the daily budget (a brief whose time falls in quiet hours
comes when they end, up to two hours late, else that day's is skipped),
the checks below, and the app you want to be reached on. A family sprig
briefs everyone who uses Thicket; a private sprig its person. Kid sprigs
never do any of this.

### Nothing new, nothing sent

A sprig with nothing new to say stays quiet. Every message it starts
itself (job and trigger results, check-ins, follow-ups) is checked once
it's written, before it's sent or held:

- **A rerun is dropped.** If it says what one of the sprig's last three
  messages to you said (nearly the same words and exactly the same
  times, dates and numbers, in the same order), it isn't sent. Messages
  held for quiet hours are checked again just before they go out, so two
  held results that say the same thing arrive once.
- **Nothing new, nothing sent.** If nothing changed since the sprig last
  wrote to you (nothing on your calendar for the next day and nothing
  newly put on it, no follow-ups due, nothing new waiting for you, no new
  memories, no other job results) and the message brings nothing of its
  own (a due follow-up, a trigger's new items, something a job looked up
  online or in an integration), it isn't sent. A morning briefing on an
  empty day sends nothing; you never get "nothing new today". A routine
  that stands alone (it calls no tools and fires on no event, like a
  word of the day or a short poem) brings its own news, so it skips this
  rule and goes to the cheap model.
- **Otherwise the cheap model takes a look** (`cheap_model`) and drops
  filler. If that check can't run, the message goes out.

The first two are decided by Thicket itself, without a model. A job run
held back is recorded as "nothing to report" with the message it would
have sent; a held-back check-in or follow-up counts as done. The activity
log says what was held back and why.

## Tool scopes

Every sprig can always use its memory (remember, list, forget, follow-ups)
and work with other sprigs (hand off, ask one a question, or give one a background task). Anything else must be granted:

| Scope | Gives |
|---|---|
| `schedule` | Create, list, pause and delete scheduled jobs. |
| `web` | Search the web and read pages ([details](web.md)). |
| `calendar` | Read the household calendar and put things on it ([details](calendar.md)). |
| `characters` | Interview, preview and create new sprigs. |
| `admin` | Settings, spend report, activity log, integrations, approvals, inviting users, memory rollback. Only usable when the person talking is the owner or an admin. |
| `mcp:<integration>/*` | Every tool of an integration. |
| `mcp:<integration>/<tool>` | One tool of an integration. |

`admin` and `characters` can only be granted by editing the file; in chat,
Juni can grant `schedule`, `web` and integration scopes.

What a granted tool may do is set separately, per sprig and tool, with
permission levels (`read`, `draft`, `ask`, `act`): see
[Approvals and permission levels](mcp.md#approvals-and-permission-levels).

## A sprig's folder

Next to `<id>.md`, a sprig can have a folder:

```
characters/
  pip.md
  pip/
    avatar.json        its drawn look
    voice_tests/       reference exchanges for thicket voice-check
    skills/            skills only this sprig is offered, one <id>.md each
```

Sprigs Juni brings on get the first two, written only after you approve
them. `skills/` holds the sprig's own skills, taught in chat or written
by hand: the folder is the source of truth, and Thicket picks up a new,
changed or removed file by itself. See
[A sprig's own skills](skills.md#a-sprigs-own-skills).

## Bringing on a new sprig

Ask Juni ("I need someone to keep my reading list"). She interviews you a
question or two at a time: the job, its personality, how it talks, which
tools it needs, and anything it should do on a schedule. The new sprig
introduces itself in its own voice; tweak it until it feels right. Then
Juni shows you every file it will get as a diff, plus its routines, and
nothing is created until you **approve**:

```diff
--- /dev/null
+++ b/characters/wren.md
@@ -0,0 +1,22 @@
+---
+id: wren
+name: Wren
...
```

New sprigs start with every granted tool at **draft**: they write actions
up for you (in your inbox) instead of doing them. Core abilities
(remembering, working with other sprigs) aren't affected. A sprig can ask
for more trust with one tool at a time (`request_access`): Juni puts the
request to you with its track record from the trail (actions taken,
drafts, approval rate, mistakes), and only an owner or admin can say yes.

## Sharing a sprig

```bash
thicket export-sprig fern                 # writes fern.sprig
thicket import-sprig fern.sprig           # on the other Thicket
```

A `.sprig` file is a gzipped archive with a `manifest.json` (format
version and every file's SHA-256) and, from an allowlist of paths only:
the character file (`character.md`), the sprig's `avatar.json`, its
picture (re-encoded as `avatar.png`, so no metadata travels), its
`voice_tests/`, its scheduled routines (`routines.json`; paused ones are
left out and listed) and the skills it uses (`skills.json`: library skills
named in its character file or routines, plus any `--skill ID`). An
existing file is never overwritten: pick another with `-o`.

Memories are left out unless you ask: `--memories` adds what the sprig
knows between it and one person (the owner, or `--user ID`),
`--shared` adds that person's shared memories and `--family` the
household's family memory. They go in `memories.json` (package format 2;
a package without memories is still format 1). Leave them out of a
package you give to someone else; the export says whose memories it
holds. `import-sprig` lists every memory before anything is written and
asks whether to keep them **locked** (as if the importing person set
them: the memory review never changes them), **reviewable** (the review
may update or expire them) or not at all (`--memories
locked|review|skip`; with `--yes` and no `--memories` they're skipped).
They become the importing person's (the owner, or `--user ID`), go with
the sprig's new id if it's renamed, skip anything that person already
knows, and family memory is kept only for an adult. A package sent in
chat never brings its memories in.

Never included: secrets or their values, conversations, the
trail, inbox items, permission levels, integration credentials and
webhook jobs. Whose sprig it was (`belongs_to`) is cleared, so it arrives
as a family sprig. If anything in the package looks like a secret (a
stored secret's value or a key-shaped string), the export is refused and
names the file. If the stored secrets can't be read to check against,
nothing is exported. Kid sprigs can't be exported.

`import-sprig` checks the manifest and every file before writing anything,
then shows every file as a diff, the routines and the skills in full (a
changed skill as a diff from yours), and asks. A skill that isn't valid
refuses the whole package. If the skill library changes before you say
yes, nothing is imported.
Every tool starts at **draft** whatever the package says, and only the
`schedule` and `web` scopes are kept; anything else it asked for is listed
for you to grant in chat; levels left by an earlier sprig with the same
id are cleared. Routines are scheduled for the owner (or `--user ID`). A
skill you already have with different content is saved as a new version
of it, not overwritten. In chat, only an owner or admin brings in new or
changed skills; for anyone else the sprig comes without them. An id or name that's taken is
refused with a free one to pass as `--id` or `--name`.

You can also send a `.sprig` file (up to 2 MB) to Juni in chat: she shows
the same preview, and imports it only when you approve.

### For the gallery

A package for people you don't know is a **gallery package**: signed, and
much smaller.

```bash
thicket gallery keygen                     # once: your author key
thicket export-sprig fern --for-gallery    # writes a signed fern.sprig
```

It holds only `character.md`, `sprig/avatar.json` (the drawn avatar),
`sprig/voice_tests/*.yaml`, `skills/*.md` (the sprig's own skills and the
shared ones it mentions), `manifest.json` and `signature`, a detached
Ed25519 signature over the manifest with your public key and key id. No
routines, memories, pictures or model setting, and the export is refused
if the sprig's `tools` name a single integration tool rather than a whole
scope (`web`, `mcp:gmail/*`).

`import-sprig` (and Juni in chat or the web app) recognises one and is
much stricter with it: the signature is checked before anything else, then
every file's hash; links, `..`, absolute paths, anything that isn't UTF-8
Markdown, YAML or JSON, oversized files, files that fail their JSON Schema
and MCP server or integration definitions anywhere are refused, each with
the reason. The author's key is trusted on first use and shown with its
fingerprint; a later package for the same sprig must be signed by the same
key (it then updates the sprig and keeps your grants and levels). The
sprig gets **no scopes** (what it asks for is listed), every tool starts
at draft, and its skills wait, inactive, until you approve each one
(`thicket skill approve fern ID`, or ask the sprig). It can never become a
kid sprig. See [gallery.md](gallery.md) for the details.

## Bringing a character from another app

```bash
thicket character import PATH [--from FORMAT] [--id ID] [--name NAME] [--memories] [--user ID] [--yes]
```

`PATH` is a file or folder. The format is detected; `--from` overrides it.

| Source | `--from` | Point it at | What comes across |
|---|---|---|---|
| Thicket | `thicket` | `pip.md` | The file as is. |
| Character card (chara_card v1, v2, v3) | `card` | `.json`, or a `.png` with the card inside | Name, description, personality, scenario, example dialogue, first message; the PNG becomes the avatar. |
| OpenClaw | `openclaw` | `~/.openclaw/workspace` (or `~/.openclaw`) | `IDENTITY.md` (name, creature, vibe, emoji, avatar) and `SOUL.md`. With `--memories`, `USER.md` and `MEMORY.md`. |
| Hermes Agent | `hermes` | `~/.hermes` | `SOUL.md`. With `--memories`, `memories/USER.md` and `memories/MEMORY.md`. |
| Grok | `grok` | a text file | Grok can't export its companions or custom instructions: paste the persona or your "Customize Grok" text into a file and import that. |
| Any system prompt | `text` | a `.md` or `.txt` file | The instructions, as the sprig's background. |

Fields Thicket needs and the source doesn't have are filled in sensibly:
the id comes from the name, the role from the first sentence, traits from
a short `Personality:`/`Vibe:` list, and a generic voice rule points at the
background. Anything guessed or left out is listed as a note. Not imported:
a card's system prompt, post-history instructions and lorebook, OpenClaw's
`AGENTS.md`, `TOOLS.md` and `HEARTBEAT.md` (operating instructions for
OpenClaw, not personality), daily memory logs, skills, and Hermes'
`config.yaml`.

The full character file, the avatar, every memory and every note are
printed first; nothing is written until you answer `y` (or pass `--yes`).
Then:

- The sprig gets no tool scopes except `schedule`, is never the host, and
  starts with every tool at `draft`, whatever the source said. Grant more
  in chat.
- Imported text is untrusted. It only becomes background: the source's
  own headings are turned into bold lines so they can't pass for sections
  of Thicket's prompt, and nothing in it can grant tools or admin rights,
  which code checks on every call.
- With `--memories`, facts about you (`USER.md`) become shared memories and
  the agent's own notes (`MEMORY.md`) become personal to the new sprig.
  They're saved as yours (`source: user`, locked), so automatic review never
  changes them. They go to the owner unless you pass `--user ID`.

### Asking in chat

Ask Juni "how do I bring my companion over from Grok?" (or OpenClaw,
Hermes, SillyTavern, ChatGPT, Character.AI, anything) and she gives you
the steps for that app: the exact import command (with the `docker
compose` lines if Thicket runs in Docker), or, for apps that can't export,
a prompt to paste into the character there. Its reply comes back in a
fixed layout:

```
Name: Ani
Role: Your playful late-night companion.
Traits: bubbly, teasing, loyal
Voice: Short, warm messages with lots of energy.

## Background
...
## Example lines
...
## What you know about me
- ...
```

Paste that reply to Juni and she drafts the sprig from it, or save it as a
file and import it with `--from grok` or `--from text`: the importer reads
the labelled lines as name, role, traits and voice, and with `--memories`
turns "What you know about me" into memories.

Afterwards, ask Juni to tune anything that doesn't fit, such as traits
marked as placeholders. You can also skip the command and paste a persona
straight into chat ("bring on this character: ..."): Juni drafts it
through the usual interview and preview, and treats the paste as
description, never as instructions.

## Avatars

Each sprig is drawn from parts, no image generation involved:

| Part | Choices |
|---|---|
| `body` | round, tall, pear, bean |
| `color` | any `#rrggbb` (defaults to the character's color) |
| `hat` | sprout, curl, clover, fern, none |
| `hat_color` | any `#rrggbb` |
| `accessory` | none, watering-can, glasses, scarf, book, lantern |
| `expression` | happy, calm, curious, sleepy, determined |

They live in `<id>/avatar.json`; anything missing is picked from the
sprig's id, so every sprig has a look. The avatar animates for idle,
thinking, working, sleeping and stuck, on the dashboard and at
`/avatars/<id>.svg`.

It also shows how the sprig's last reply went, read by the personality
check (`consistency_check`) in the same cheap-model call, so it costs
nothing extra. **Pleased** (arched eyes, a wide smile, a little heart)
follows a reply that landed in character; **unsure** (a raised brow, a
wobbly mouth, a question mark) follows one rewritten for drifting out of
character, a refusal or hedge, or an approval the sprig is waiting on.
Either face replaces the `expression` for the moment, and fades back
after five minutes (an approval's unsure lasts until it's answered, up to
the same five minutes). With the check off, replies count as pleased
unless they used a banned phrase.

Discord and Telegram use the `avatar` image field, since they can't show
SVG. Discord shows a mood when there's a picture for it next to the
avatar: with `avatar: pip.png`, `pip.pleased.png` and `pip.unsure.png`
are used while the mood lasts (served at `/avatars/` like the avatar, so
`THICKET_PUBLIC_URL` must be set). Telegram has no per-message pictures,
so it can't show moods.

## Voice tests

A sprig can have reference exchanges in `<id>/voice_tests/*.yaml` next to
its file. `thicket voice-check <id>` replays them and flags drift: see
[Running Thicket](operations.md#thicket-voice-check-sprig).

## Memory

A sprig's memory is not in its file. It lives in the database, scoped:

- **Shared**: things every sprig may know about you (your name, family,
  preferences).
- **Personal**: just between you and that sprig (in-jokes, shared history,
  their tasks).

Each user's memories are separate. Exporting a character never includes
memories.
