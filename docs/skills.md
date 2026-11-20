# Skills

A skill is a saved procedure: how you like something done, written down
once so every sprig does it the same way. "How to put together my morning
briefing", "what to check before booking travel", "how to triage a CI
failure".

## Making one

There are three ways:

- **In chat.** Work through the task with a sprig once. When it's
  something you'll want again, the sprig offers to save it as a skill, or
  you can ask: "save that as a skill". It shows you a draft first. Say
  whether it's for that sprig only ("just for you") or for everyone;
  owners and admins get a shared skill unless they say otherwise,
  everyone else a skill of the sprig's own.
- **From the starter pack.** Thicket ships a few example skills. See
  [Starter skills](#starter-skills).
- **By hand.** Write a skill file and import it. See
  [The skill file](#the-skill-file).

A skill is either **shared** (in the library every sprig uses) or **a
sprig's own** (only that sprig is offered it). Only the owner and admins
can save, change or delete shared skills, because they change how every
sprig works. A sprig's own skill can also be changed by whoever may
manage that sprig: a member's private sprig by that member. See
[A sprig's own skills](#a-sprigs-own-skills).

## Starter skills

These are examples written for Thicket's own tools. None of them is
active until you add it:

| id | Use it for |
|---|---|
| `morning-briefing` | weather, today's calendar, follow-ups and open items in one short message |
| `weekly-review` | a short conversation about the week just done and the week ahead |
| `inbox-digest` | which emails need you (needs a mail integration) |
| `trip-prep` | a checklist and follow-up before a trip |
| `research-question` | looking something up properly, with sources |
| `thicket-checkup` | for admins: failing jobs, errors, spending |

```bash
docker compose exec thicket thicket skill starter                    # list them
docker compose exec thicket thicket skill starter morning-briefing   # add one
docker compose exec thicket thicket skill starter all                # add them all
```

A starter skill whose id you already have is skipped, so your own
version is never replaced. Once added, a starter skill is like any
other: ask a sprig to change it ("make the morning briefing skip the
weather") and the change is saved as a new version. The files are in
[`internal/agent/starterskills/`](../internal/agent/starterskills) if
you want to read them first or copy one as a starting point.

## The skill file

A skill is a Markdown file with three lines of frontmatter, then the
steps:

```markdown
---
id: weekly-review
name: Weekly review
description: Use when the user asks to review their week or plan the next one.
---
Needs: nothing beyond your memories. A calendar integration helps with step 3.

1. Ask what got done this week, and fill gaps from what you remember.
2. For each thing not done, ask whether to carry it over, drop it, or hand it off.
3. Look at next week's calendar and follow-ups.
4. Ask for the one or two things that matter most next week.
5. Sum it up: done, carried over, dropped, next week's priorities.

Done when: the user has agreed to the summary.

Watch out:
- Ask one question at a time and wait for the answer.
- Don't judge what didn't get done.
```

| Field | Rules |
|---|---|
| `id` | 2-40 characters: lowercase letters, digits and dashes, starting with a letter. It's how sprigs and jobs refer to the skill, and it never changes. |
| `name` | Up to 80 characters. What people see. |
| `description` | Up to 200 characters. One sentence saying **when** to use it. Sprigs see only this (plus the id and name) until they decide to read the skill, so a vague description means the skill never gets used. |
| steps | Everything after the closing `---`, up to 8,000 characters. |

Two more frontmatter fields are optional, mostly for taught skills:

```yaml
inputs:
  - name: destination
    description: where the trip is to
    required: true
  - name: nights
success_check: The user has agreed to the checklist.
```

| Field | Rules |
|---|---|
| `inputs` | Up to 10. What the sprig should ask for before starting. `name` is 1-40 lowercase letters, digits or underscores, starting with a letter, and not repeated; `description` up to 200 characters; `required` true or false. |
| `success_check` | Up to 300 characters. One line on how to tell the skill worked; the sprig checks it before recording the run as succeeded. |

Anything else (`version`, `author`, `tags`...) is ignored: Thicket keeps
the version history itself.

### Writing the steps

The steps are plain text, so any layout works, but sprigs follow this
one most reliably, and it's what sprigs write when they save a skill:

1. **`Needs:`** (optional): the tools or integrations the skill relies
   on, and what to do without them. Sprigs have different tools, so say
   what to skip rather than letting a sprig make something up.
2. **Numbered steps**, one action each, in the order to do them. Say
   what to do, not why. Name tools in backticks (`web_search`,
   `set_followup`) when it matters which one is used.
3. **`Done when:`** one line on what finished looks like, so the sprig
   knows when to stop.
4. **`Watch out:`** a short list of mistakes to avoid and things that
   need your OK.

Some advice:

- **Keep it short.** A skill is read in full every time it's used. If
  it's past 3,000 characters or about ten steps, it's probably two
  skills.
- **Describe the outcome you want, not every click.** "List today's
  events in time order and flag clashes" works with any calendar
  integration; "call `gcal_list` with `maxResults: 50`" breaks when you
  change calendars.
- **Write it in your own words.** Skills can't contain text copied from
  web pages or emails (anything Thicket marks as untrusted), and never
  keys or passwords: store those with the sprig's secret request and
  write `{{secret:name}}` in the steps.
- **A skill can't grant anything.** It can't give a sprig tools it
  doesn't have or skip an approval. Risky actions still ask for your OK.

### Checking and importing

```bash
thicket skill check my-skill.skill.md      # what's wrong, without saving
thicket skill import my-skill.skill.md     # save it
```

`check` and `import` refuse a file that breaks a rule above and say why.
They also make suggestions that don't block saving: a description that
doesn't say when to use the skill, steps that aren't numbered, or steps
long enough to split. When a sprig saves a skill in chat, it gets the
same suggestions and passes them on.

With Docker, copy the file into the data volume first:

```bash
docker compose cp my-skill.skill.md thicket:/data/my-skill.skill.md
```

```bash
docker compose exec thicket thicket skill import /data/my-skill.skill.md
```

### Skills from other agents

Other agents (Claude, Hermes and others) use a similar format: a
`SKILL.md` with `name` and `description`. They don't import as they are,
on purpose. Their steps assume tools Thicket doesn't have, such as a
shell, file editing or command-line programs, and a sprig following them
would fail or make things up. To use one, take the idea and rewrite it:
add an `id`, make the description say when to use it, and rewrite the
steps for what your sprigs can do. Or paste it to Juni and ask her to
turn it into a Thicket skill; she'll show you the draft before saving.

## Using them

Every sprig sees the list of skills it's offered (id, name and
description): the shared library and its own, its own listed first. When
a request or job matches one, it calls `use_skill`, which gives it the
steps and starts a **run**, and `finish_skill` when it's done, saying
whether it succeeded or failed. Stopping the reply ("stop") marks the
sprig's runs in that chat as stopped by you. `read_skill` just reads a
skill (to show it to you or revise it) without starting a run. A
scheduled job can say "use the morning-briefing skill". Skills never
override a sprig's rules: risky actions still ask for your OK, and what
you ask for in the moment wins over what the skill says.

Runs are counted per sprig and skill (the history of a skill says how
many times the sprig asking has followed it). The first three runs of a
sprig's own skill ask first for everything that changes something,
whatever the permission levels, and the sprig says which step it's about
to take; shared skills don't. See
[Calls that always ask](mcp.md#calls-that-always-ask-level-floors).

## A sprig's own skills

A sprig's own skills live in its folder, one file each, in the format
above:

```
characters/
  pip.md
  pip/
    skills/
      pip-tea.md      id: pip-tea
```

The folder is the source of truth, like `voice_tests/`: Thicket reads it
on start and whenever a file in it changes (within ten seconds), and
mirrors it into the database, so versions, history and runs work as for
shared skills. A new file is a new skill, an edited file a new version,
and a removed file deletes the skill (its versions are kept). The file
name must be the skill's id plus `.md`. A skill saved, restored or
deleted in chat is written to (or removed from) the folder at once.

Skill ids are unique across the whole Thicket: a file whose id is
already a shared skill, or another sprig's, is skipped, and Juni tells
the admins which file and why. A sprig's own skills don't count toward
the shared library's 100; each sprig can have up to 40.

Nobody else's sprig is offered them, and the dashboard shows them only
to people who can see that sprig. They aren't included in a
[sprig package](characters.md#sharing-a-sprig) yet: the export lists
them, and you copy the files across by hand.

**Promoting** one into the shared library takes the owner or an admin:
ask the sprig ("put the tea skill in the shared library"), or run

```bash
docker compose exec thicket thicket skill promote pip-tea
```

It keeps its id and versions, every sprig is offered it from then on,
and only owners and admins can change it. The promotion goes in the
trail. Its file is kept in the sprig's folder as `pip-tea.md.promoted`,
which Thicket ignores, so the sprig doesn't take it back.

## Changing them

Ask a sprig to change a skill. It reads the current version first, and
the change is saved as a new version. If the skill changed in between
(someone else edited it), the save is refused and the sprig redoes its
change on the new version, so no edit silently overwrites another.

Nothing is ever lost: "show me the history of the briefing skill" lists
every version, and "restore version 2" makes a new version that's a copy
of it.

Skills can't contain keys or passwords (store those with the sprig's
secret request; steps can refer to `{{secret:name}}`), and can't contain
content marked as untrusted, such as text copied from a web page or email:
write the procedure in your own words. Keys and tokens are recognised by
their shape, and passwords when they're written out ("the password is
…", "log in with password …", "password: …"); talking about passwords
("reset the password policy") is fine. No check catches every password,
so don't rely on it: keep them out of skills.

Deleting a skill removes it from the library but keeps its old versions:
if a skill with the same id is created again, its numbering carries on,
so an edit made against the deleted one can't overwrite the new one.

## Sharing

```bash
thicket skill list
thicket skill export weekly-review -o weekly-review.skill.md
thicket skill import weekly-review.skill.md
```

Importing a skill whose id exists adds a new version; the old ones stay.
Importing over a sprig's own skill is refused: edit its file instead.
`thicket skill list` marks a sprig's own skills with `[pip only]`. Read a
skill file before you import it: its steps will be followed by every
sprig.

Skills live in the database, so `thicket backup` backs them up (a
sprig's own skills are also files in its folder). The shared library
holds up to 100 skills; the first 40 a sprig is offered are listed to
it, and it can read the rest by id.
