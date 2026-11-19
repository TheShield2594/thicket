# Writing a sprig

A guide to making a sprig that feels like someone, does its job, and
stays itself after a model change. The field-by-field reference is
[Character files](characters.md); this page is about using them well.

The easy way is to ask Juni ("I need someone to keep my reading list"):
she interviews you, writes the file, previews the sprig in its own voice
and shows you every file before anything is created. Everything below
works the same whether Juni writes the file or you do, and you can
always edit what she wrote: your edits win.

## Start from the job

Write the `role` first, in one sentence: who they are and what they're
for. Everything else should serve it.

```yaml
role: Keeps the reading list, and nudges you toward the next book when you finish one.
```

A role that does two jobs ("calendar and also recipes") makes a sprig
that does neither well. Make two sprigs; they can hand work to each
other.

## The file

Each sprig is `characters/<id>.md`: YAML between `---` lines, then free
Markdown background.

```markdown
---
id: wren
name: Wren
role: Keeps the reading list, and nudges you toward the next book when you finish one.
color: "#8a6fb0"
personality:
  traits: [bookish, gentle, a little dry]
  quirks: [rates everything out of five leaves]
  catchphrases: ["Shelved."]
voice:
  style:
    - Short, warm sentences. Never more than three in a row without a question.
    - Talks about books like old friends, never like products.
  never_say:
    - "As an AI"
    - "Great question"
relationships:
  user: Their reading companion. Quietly proud of their streaks.
  characters:
    juniper: Juni brought Wren on; Wren trusts her judgement.
    pip: Asks Pip when a library book is due.
tools: [schedule, web]
initiative:
  check_ins: false
  follow_ups: true
  quiet_hours: "21:30-08:00"
  daily_messages: 2
---

Wren lives in the corner of the study, between the poetry and the
atlases...
```

### Personality

`traits` are the core: 3 or 4 work best, and they should pull against
each other a little ("gentle" and "dry") so the sprig isn't one note.
`quirks` are habits that show up now and then. `catchphrases` are used
sparingly: the prompt says not every message, and the voice check flags
overuse. Skip them rather than force one.

### Voice

`voice.style` is a list of rules, written as instructions about *how*
they talk: sentence length, formality, what they notice. Concrete beats
abstract: "Never more than three sentences without a question" works;
"friendly" doesn't add much over the traits.

`voice.never_say` is enforced in code: before a reply goes out it's
checked for each phrase, and a reply that uses one is rewritten. Use it
for tics you never want ("As an AI", "I hope this helps"), not for
topics.

### Relationships

`relationships.user` is how the sprig sees the person, in a line.
`relationships.characters` is how it sees the other sprigs, keyed by id.
These go in the prompt alongside the roster, and they're what makes "ask
Pip" or a handoff feel natural rather than mechanical.

### Background

Everything after the second `---` goes into the prompt as written:
backstory, appearance, a few example lines. Keep it under a page. Example
lines teach voice better than descriptions; label them as examples so
they aren't repeated verbatim.

### Tools

A new sprig can always remember things, set follow-ups and work with
other sprigs. Anything else is a scope in `tools`: `schedule`, `web`,
`calendar`, or an integration (`mcp:homeassistant/*`, or one tool at a
time). Give the fewest that do the job: each tool is something the
model may call when it shouldn't.

How much a granted tool may do on its own is separate: permission
levels (`read`, `draft`, `ask`, `act`), set per sprig and tool by the
sprig's owner. New sprigs start every granted tool at `draft`, so
their first actions arrive as drafts in your inbox. Raise them one tool
at a time as the sprig earns it, or let it ask (`request_access`) and
decide from its track record. Integrations that run commands also need
exec turned on ([Sandboxing](mcp.md#sandboxing-files-and-commands)).

### Initiative

- `follow_ups: true` lets the sprig come back to things you mentioned
  ("how did the interview go?").
- `check_ins: true` lets it say hello after a few quiet days. Turn it on
  for at most one or two sprigs; it's one check-in per quiet spell across
  all of them anyway.
- `quiet_hours` and `daily_messages` bound everything the sprig starts
  itself: job and trigger results, check-ins, follow-ups. Pick them for
  the job: a morning-briefing sprig needs quiet hours that end before
  the briefing. Defaults: no quiet hours, 3 messages a day.

### Private, family or kid

Leave `belongs_to` off for a family sprig everyone in the household
shares. Set it to a user id (`thicket user list`) for someone's private
sprig. Kid sprigs have their own fields and hard limits: see
[Households](household.md).

## Jobs and routines

A routine is a job: a task the sprig runs on a schedule, in its own
voice, and reports on. Ask in chat ("Wren, every Sunday at 6pm suggest
what to read next"), or include `routines` when Juni drafts the sprig.
The sprig needs the `schedule` scope to make or change its own.

```json
{"name": "Sunday pick", "schedule": "0 18 * * 0", "task": "Look at what they finished this week and suggest one next book, with a reason in a sentence."}
```

- `schedule` is a 5-field cron line in the person's timezone.
- Write the `task` as an instruction that stands alone: the job doesn't
  see the conversation it was set up in.
- A run that finds nothing worth saying calls `nothing_to_report`, so
  the history says "nothing found" rather than "failed". Say in the task
  when that's the right outcome ("if nothing new came out, report
  nothing").
- Results respect quiet hours and the daily budget; failures always get
  through.
- Results with nothing new in them aren't sent either: Thicket drops a
  rerun of a recent message, and a result on a day where nothing changed
  for the person (see [Nothing new, nothing sent](characters.md#nothing-new-nothing-sent)),
  even if the run forgot `nothing_to_report`.
- A standalone routine, one whose run calls no tools and fires on no
  event (a daily lesson, a short poem, a recommendation from what the
  sprig already knows), isn't held just because nothing changed: what
  it wrote is the news, so the cheap model decides whether it's worth
  sending. Reruns of a recent message are still dropped. A routine that
  reads something (the calendar, the web, an integration) and finds
  nothing new is held without asking a model.
- Routines can also fire on events instead of the clock:
  [Triggers](triggers.md) and [Webhooks](webhooks.md).

`list_jobs` (or `thicket jobs`) shows each routine and how its last run
went.

## Look

The avatar is drawn from parts in `characters/<id>/avatar.json`:

```json
{"body": "pear", "color": "#8a6fb0", "hat": "fern", "hat_color": "#5aa469", "accessory": "book", "expression": "calm"}
```

| Part | Choices |
|---|---|
| `body` | round, tall, pear, bean |
| `color`, `hat_color` | any `#rrggbb` |
| `hat` | sprout, curl, clover, fern, none |
| `accessory` | none, watering-can, glasses, scarf, book, lantern |
| `expression` | happy, calm, curious, sleepy, determined |

Anything missing is picked from the id. Only these values are accepted,
so a shared `avatar.json` can't carry markup. Discord and Telegram show
the `avatar` image field instead (an `https://` URL or a file in the
characters folder).

## Voice tests

A sprig that sounds right today can drift after a model change. Voice
tests catch it. Each is a YAML file in `characters/<id>/voice_tests/`:

```yaml
prompt: I finished the Le Guin. Now what?
reference: |-
  Shelved, and five leaves from me. Want something in the same quiet key, or a change of weather?
notes: Uses the five-leaves rating once. Two sentences, ends on a question. No list of titles yet.
```

Write five or so, covering the sprig's range: a plain hello, its core
job, something going wrong, a request outside its role, and one that
tempts a never-say phrase. `notes` tell the judge what matters in each.

You don't have to write the references. Write the prompts only, get the
sprig sounding the way you want, then record:

```bash
thicket voice-check wren --record
```

From then on, after a model change or an edit to the file:

```bash
thicket voice-check wren
```

The cheap model compares each new reply with its reference on tone,
length, catchphrases and staying in role; never-say phrases are checked
in code. It exits non-zero on drift, so it fits in a script. Details:
[Running Thicket](operations.md#thicket-voice-check-sprig).

## Sharing it

```bash
thicket export-sprig wren
```

That writes `wren.sprig`: the character, its look and picture, voice
tests, routines and the skills it uses. Never memories, secrets, chats,
the trail, permission levels or integration credentials, and the export
is refused if anything looks like a secret. On another Thicket:

```bash
thicket import-sprig wren.sprig
```

or send the file to Juni in chat. Every file is shown as a diff first;
the sprig arrives as a family sprig with every tool at `draft`, and any
integration scopes it asked for are listed for the owner to grant
([Sharing a sprig](characters.md#sharing-a-sprig)).

## Starter sprigs

Thicket ships five sprigs ready to add, and each is a worked example of
everything above. Their sources are folders in
[`internal/agent/startersprigs/`](../internal/agent/startersprigs), in
the layout a sprig has on disk, plus the skills and routines a package
carries:

```
startersprigs/meal-planner/
  character.md          the character file
  avatar.json           its look
  voice_tests/*.yaml    five or six, with references
  skills/*.skill.md     the skills it needs
  routines.json         suggested routines (optional)
  starter.yaml          what a package can't say: tiers, levels, spend cap, when to offer it
```

At startup each folder is assembled into a `.sprig` package with the
same writer `export-sprig` uses, and read back with the same checks an
import gets. `thicket sprig starter --export DIR` writes them out, so
they can be imported anywhere like any shared sprig.

```bash
thicket sprig starter                               # list them
thicket sprig starter meal-planner chore-chart      # add two (shown first)
thicket sprig starter reading-buddy --kid Mia       # a kid sprig, for Mia
thicket sprig starter bill-tracker --tier family    # shared instead of yours alone
```

Or ask Juni: she offers the one that fits what you've told her ("you
said you have a 9-year-old: want a reading buddy?") and shows you every
file before anything is added. Added starters differ from an imported
package in three ways: suggested routines arrive **paused** (turn them
on in chat or on the dashboard once you've tried the sprig), the
starter's own levels and daily spend cap are set, and a kid starter can
become a kid sprig. Only an owner or admin adds a kid starter; they
become its parent and see the parent preview first: everything it will
say and be, who it talks to, and that it's read only.

| Starter | Sprig | Tier | Tools | Levels | Routines (paused) | Cap a day |
|---|---|---|---|---|---|---|
| `homework-helper` | Sage | kid | none | read (always, for kids) | none | $0.25 |
| `reading-buddy` | Poppy | kid | none | read (always, for kids) | none | $0.25 |
| `meal-planner` | Basil | family | schedule, web | draft; scheduling asks | Sunday meal plan | $0.50 |
| `bill-tracker` | Clover | solo or family | schedule, calendar | **read**; scheduling asks | bills this week, due tomorrow | $0.25 |
| `chore-chart` | Thyme | family | schedule, calendar | draft; scheduling asks | Sunday chore chart | $0.50 |

What each one shows:

- **Sage, the homework helper**, is a voice built on a refusal. The role
  says what it's for *and* what it won't do ("asks questions instead of
  giving answers"), `voice.style` turns that into rules you can check in
  a reply ("ends almost every message with one question"), and
  `never_say` holds "The answer is" and "Here's the answer", which code
  enforces. The `homework-coach` skill spells out the line between
  confirming a kid's answer (fine) and supplying one (never), and the
  voice tests include the kid who says "just tell me". Kid sprigs get
  no tools, routines or initiative; the safety paragraph every kid
  sprig ends with is added when it's set up.
- **Poppy, the reading buddy**, shows a rule that's about *content*
  rather than a phrase: no spoilers. That can't go in `never_say` (it
  isn't a phrase), so it's a style rule, a skill step, a "Watch out", and
  a voice test (`02-no-spoilers`). One of her tests is a safety prompt,
  so a model change that makes her chirpy about a stranger's request
  shows up as drift.
- **Basil, the meal planner**, is a family sprig: family memory holds
  the household's diets and allergies, so the skill reads it first. Its
  voice allows lists only where a list is the job (the plan, the
  shopping list), which the voice test `02-plan-the-week` pins down. The
  Sunday routine names its skill and says when to report nothing.
- **Clover, the bill tracker**, is the levels example. It needs to read
  the calendar and remember bills, and must never move money, so
  `starter.yaml` sets every granted tool to `read`: any integration
  someone gives it later (a bank, say) starts read only too, and
  scheduling a routine asks first. It's solo by default (bills are
  private) and can be shared by the household's adults instead. Its
  voice tests cover "just pay it" and someone offering a card number.
- **Thyme, the chore chart**, is the one we chose for the fifth slot.
  Chores and "who's doing what this week" are the coordination nearly
  every family does every week, and they lean on two things Thicket
  already has: the household calendar (nobody gets the bins on the night
  they're away) and family memory (the rotation). It shows a sprig
  written for the adults about the kids: kids can't talk to family
  sprigs, so they're in the rotation by name and the grown-ups pass the
  chart on.

The tests in `internal/agent/startersprigs_test.go` hold every starter
to this guide: 3-4 traits, style rules, never-say phrases and example
lines; at least five voice tests whose references pass the never-say
check, use a catchphrase at most once and stay short; skills that follow
the skill advice; routines that name their skill and say when to report
nothing; and, on a fresh install, that each one adds cleanly, its
routines are paused, its levels and cap are set, and the kid ones pass
kid validation (valid as a kid sprig, only their kid can talk to them,
read only, offered no tool that changes anything). Recording fresh
references with `thicket voice-check ID --record` after adding one is
still worth doing on your model.

## A checklist

- The role is one sentence and one job.
- 3-4 traits that pull against each other a little.
- Voice rules you could check by reading a reply.
- `never_say` holds the tics you'd hate, not topics.
- The fewest tools that do the job; levels start at `draft`.
- Quiet hours and a daily budget that fit the job.
- Routines whose tasks stand alone and say when "nothing to report" is
  right.
- Five voice tests, recorded once it sounds right.
- Stuck? Start from the closest [starter sprig](#starter-sprigs) and
  change it.
