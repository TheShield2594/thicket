# Households: solo, family and kid sprigs

Thicket works for one person or a whole household. Each person has their
own memories and their own sprigs, the household shares some sprigs, and
kids get sprigs of their own that are locked down in code.

## Roles

| Role | Who | Can |
|---|---|---|
| owner | whoever set Thicket up | everything an admin can; their role never changes |
| admin | household admins | invite people, change roles, create kid sprigs, change permission levels, connect integrations |
| member | adults | talk to family sprigs and their own solo sprigs, connect their calendars, approve what their sprigs do |
| kid | kids | talk only to the kid sprigs set up for them |

`admin` stays a role of its own rather than folding into `owner`: a
household often has two adults who both run things, but only one
owner, whose role can't be changed and who alone can pause everything.

Admins add people by asking Juni ("add my wife's sprig", "invite my
housemate", "invite 1234567890 on Discord as a kid"), from the dashboard's
invite link, or with the app API (`POST /api/v2/household/invites` with
`"role": "kid"`). Role changes go in the [trail](#the-trail) and can be
undone.

## Adding someone, start to finish

Ask Juni: "add my partner's sprig". She asks their name and how they'll
chat, and posts one invite link (valid 7 days, single use) for you to send
them privately:

- **Web app**: the link opens the invite page, where they choose a
  username and password.
- **Telegram**: the link opens a chat with the bot; pressing **Start**
  uses the invite.
- **Discord**: they need to share a server with the bot. They DM the bot
  the `join-…` code from the invite.

As soon as they arrive, Juni greets *them* (never you): she learns their
name, explains in two lines what's private and what's shared, introduces
the family sprigs, points them to the calendar, and offers to make them a
sprig of their own. If the invite asked for one ("add my partner's
sprig", or `own_sprig` in the API), she goes straight to that interview.
You never see the interview, the preview, the approval or the sprig.

The same short tour happens on the first message of anyone who joins an
existing household some other way (an id you gave Juni, a chat app
account added later), admins included: only the owner, and admins set up
alongside them, get the full setup tour. The family sprigs she introduces
are listed by Thicket, never anyone's private sprigs. It happens once:
when they've had it (or skip it), it's done.

Kids are greeted by their kid sprig instead: a short, kid-sized hello on
arrival if they came through an invite, else on their first message, and
only once. Nothing about grown-up features, and Juni never talks to kids.

## Private means private

Another adult's solo sprig is theirs alone, whoever you are:

- The dashboard shows "Kim's private sprig": that it exists, its state,
  whether it has logged errors in the last day, and under Costs what it
  spends. Not its name, role, tools, activity or changes.
- The app API leaves it out of the trail and lists it in the household
  only as `{"tier": "solo", "private": true, "belongs_to": 3}`. Its live
  state isn't sent to anyone else's dashboard.
- In chat, Juni's activity and spend reports label it the same way, and
  admins can't change its tools or permission levels.
- Memories and chats were already per person; nobody else can read them.

Whoever runs the server can technically open the database file, which
holds everything. Thicket won't show it to them, and Juni says so plainly
when you add someone.

## Who may change a sprig

Every sprig has an owner, and only its owner changes what it may do:
permission levels, trust upgrades, exec, granted paths, and pausing it.

| Sprig | Its owner |
|---|---|
| solo | the person it belongs to (not admins, not the household owner) |
| family | the household owner and admins |
| kid | the household owner and admins (it stays locked to read whatever they set) |

So a member can change their own solo sprigs' levels ("Juni, let Wren
add to my reading list without asking") but not a family sprig's or
anyone else's. Every refused attempt goes in the [trail](#the-trail) as
`refused` and in the activity log; one aimed at someone else's private
sprig doesn't name it. Giving a sprig an integration (`grant_tools` with
`mcp:…`) stays with admins, since integrations hold the household's
accounts.

Approvals, drafts and needs-you items go to the sprig's owner: for a solo
sprig its person; for a shared sprig, the person whose request caused
them (Kim's request to the family meal planner drafts into Kim's inbox,
not the owner's).

## Three kinds of sprig

**Solo sprigs** are one person's: a comics tracker, a homelab helper. Set
`belongs_to` to their user id. Nobody else can talk to them, and nobody
else can see what they are or do, admins and the owner included: see
[Private means private](#private-means-private).

**Family sprigs** are the household's: a meal planner, a chores keeper.
Every adult talks to them, and they keep **family memory**: what they learn
about the household ("we're vegetarian", "bins go out on Thursday") with
`remember` scope `family`. Family memory is separate from everyone's
private memory: a family sprig talking to Kim sees family memory and
Kim's own memories, never Sam's. Family sprigs put things on the shared
household calendar. Family memory is saved with `remember` scope
`family`, or by the memory review when an adult's conversation with
family sprigs goes quiet: household facts said in passing land there,
facts about the person stay in their own memory. A kid's conversation or
a solo sprig's never adds to family memory, and `thicket memory rollback`
undoes the review's family changes like any other. The sprig is told which layer each memory comes
from (`family`, the person's `shared` memories, or `yours` between it and
the person), so it doesn't treat the household's as private or the
person's as the household's.

**Kid sprigs** talk to the kids they were set up for:

```yaml
---
id: bramble
name: Bramble
role: A friendly companion for Max.
household: kid
kids: [7]        # Max's user id
parents: [1, 2]  # who set it up
tools: ["mcp:fortnite/player_stats"]   # tools a parent approved, one by one
allow: []        # family_memory, calendar and/or voice, if a parent allows
personality:
  traits: [playful, kind]
voice:
  style: [short sentences a kid can follow]
---
```

Admins create them (`POST /api/v2/kid-sprigs`, or write the file). Their
parents then decide what they may use (`PATCH /api/v2/kid-sprigs/bramble/access`).

## What kid sprigs can't do

These are enforced in code, whatever the model or the file says. Tests
cover each one.

- **Chat only, locked to read.** Any tool that changes something is
  refused, whatever permission level is stored, and levels for a kid
  sprig can't be raised. Nothing asks for approval.
- **Only tools a parent approved,** one at a time: `web`, or a single
  integration tool such as `mcp:fortnite/player_stats`. Whole
  integrations (`mcp:gmail/*`) are refused, and even an approved tool is
  only offered if it only reads.
- **No jobs, routines, triggers, webhooks, background tasks, follow-ups or
  check-ins.** A job owned by a kid sprig refuses to run.
- **No adult data** (calendars, email, family memory) unless a parent
  allows it in `allow`. With `calendar` allowed they see only shared
  household events, never a parent's own calendar.
- **No talking out loud** unless a parent allows `voice`: then the kid
  sprig answers a voice message with one, like any sprig.
- **Kids talk only to their kid sprigs.** Naming Juni or a family sprig
  doesn't reach it, and adults can't chat with a kid sprig.
- **Relationship memory stays in the sandbox.** How the kid likes their
  kid sprig to talk, what they enjoy and what annoyed them belongs to
  that kid sprig alone: never shared with other sprigs or family memory.
  The kid and the sprig's parents can see and change it on the memory
  screen; nobody else can.

## Parents

In the web app, each kid sprig you set up appears under Household in the
sidebar ("Bramble’s chats"). It shows the whole chat, read-only, and what
the sprig may use: approve or remove tools one by one, and switch family
memory or the calendar on or off. Every change goes in the trail. The
same page shows what the sprig remembers about its kids, relationship
memory apart from facts: edit, lock, undo or delete any of it, or add
("likes simple words"), here, on the dashboard, or with
`GET /api/v2/memories?sprig=bramble`.

- **Read the whole chat, any time:** in the web app, or `GET /api/v2/kid-sprigs/bramble/chats`.
  Kids' messages are kept: "delete that" and deleting in the chat app
  don't remove them, and message retention doesn't prune kids' chats.
- **No routine notifications.** Parents aren't told about ordinary chats.
- **Safety alerts.** If a kid's message touches self-harm, a stranger
  asking for personal details (where they live, their school, photos, to
  meet), signs of abuse or grooming, or bullying, each parent gets a
  message from Juni with what was said. A cheap model screens messages
  that contain a warning word; messages about self-harm alert even if
  that check fails. Alerts are at most one an hour per kid and concern, go
  in the trail, and never block the chat.

## The trail

Every permission change and every sprig-created calendar entry is recorded
with what undoes it: tool levels, tool grants, what a kid sprig may see,
role changes, invites, calendar entries. See them under **Changes** on the
dashboard (admins see everything, others what concerns them) and press
**Undo**, or use `POST /api/v2/trail/{id}/undo`. Admins undo permission
changes; parents undo changes to their kid's sprig; anyone undoes what a
sprig put on their own calendar, and an adult who took a shared entry off
the calendar can put it back. Unlike the activity log, the trail is
never pruned.
