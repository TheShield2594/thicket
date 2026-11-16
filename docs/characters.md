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

Unknown fields are an error, so typos don't silently do nothing.

## Tool scopes

Every sprig can always use its memory (remember, list, forget, follow-ups)
and work with other sprigs (hand off, ask one a question, or give one a background task). Anything else must be granted:

| Scope | Gives |
|---|---|
| `schedule` | Create, list, pause and delete scheduled jobs. |
| `characters` | Interview, preview and create new sprigs. |
| `admin` | Settings, spend report, activity log, integrations, approvals, inviting users, memory rollback. Only usable when the person talking is the owner or an admin. |
| `mcp:<integration>/*` | Every tool of an integration. |
| `mcp:<integration>/<tool>` | One tool of an integration. |

`admin` and `characters` can only be granted by editing the file; in chat,
Juni can grant `schedule` and integration scopes.

## Memory

A sprig's memory is not in its file. It lives in the database, scoped:

- **Shared**: things every sprig may know about you (your name, family,
  preferences).
- **Personal**: just between you and that sprig (in-jokes, shared history,
  their tasks).

Each user's memories are separate. Exporting a character never includes
memories.
