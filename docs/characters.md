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
| `relationships.user` | | How they see you. |
| `relationships.characters` | | How they see other sprigs, keyed by id. |
| `tools` | | Tool scopes, below. |
| `initiative.check_ins` | | May send a gentle check-in after you've been quiet for a few days (`checkin_days`, default 3). At most one per quiet spell, across all sprigs. |
| `initiative.follow_ups` | | Follows up on things you mentioned ("how did the interview go?"). |
| `initiative.quiet_hours` | | `"22:00-08:00"` in your timezone: no proactive messages then. |
| `model` | | Override the chat model for this sprig (an OpenRouter model id). |
| `created_by` | | Informational. |
| `belongs_to` | | A Thicket user id (`thicket user list`): the sprig is that person's own, and only they can talk to it, see it in the web app or its dashboard card, or hand work to it. Leave it off (the default) for a sprig everyone shares. The host is always shared. Ask Juni for a "private" sprig and she sets it to you. |
| `household` | | The sprig's tier: `solo` (one person's: needs `belongs_to`), `family` (shared by every adult; the default without `belongs_to`) or `kid`. See [Households](household.md). |
| `kids` | kid sprigs | User ids of the kids it talks to. |
| `parents` | kid sprigs | User ids of the adults who set it up: they read its chats, get its safety alerts and choose what it may use. |
| `allow` | | Kid sprigs only: adult data a parent lets it see, `family_memory` and/or `calendar`. Nothing by default. |

Unknown fields are an error, so typos don't silently do nothing.

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
```

Sprigs Juni brings on get both, written only after you approve them.

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
