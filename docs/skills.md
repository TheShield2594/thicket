# Skills

A skill is a saved procedure: how you like something done, written down
once so every sprig does it the same way. "How to put together my morning
briefing", "what to check before booking travel", "how to triage a CI
failure".

## Making one

Work through the task with a sprig once. When it's something you'll want
again, the sprig offers to save it as a skill, or you can ask: "save that
as a skill". Only the owner and admins can save, change or delete skills,
because a skill changes how every sprig works. Anyone's sprigs can use
them.

A skill has an id (`morning-briefing`), a name, a one-sentence description
of when to use it, and numbered steps in plain words.

## Using them

Every sprig sees the list of skills (id, name and description) and reads
a skill's steps when a request or job matches it. A scheduled job can say
"use the morning-briefing skill". Skills never override a sprig's rules:
risky actions still ask for your OK, and what you ask for in the moment
wins over what the skill says.

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
write the procedure in your own words.

## Sharing

Skills are Markdown files with a little frontmatter:

```markdown
---
id: weekly-review
name: Weekly review
description: Use on Sunday evenings to review the week.
---
1. List what got done.
2. Ask what to drop or delegate next week.
```

```bash
thicket skill list
thicket skill export weekly-review -o weekly-review.skill.md
thicket skill import weekly-review.skill.md
```

Importing a skill whose id exists adds a new version; the old ones stay.
Read a skill file before you import it: its steps will be followed by
every sprig.

Skills live in the database, so `thicket export` backs them up. A library
holds up to 100 skills; the first 40 are listed to sprigs, and they can
read the rest by id.
