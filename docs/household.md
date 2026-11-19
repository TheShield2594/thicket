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

Admins add people by asking Juni ("invite 1234567890 on Discord as a kid"),
from the dashboard's invite link, or with the app API
(`POST /api/v2/household/invites` with `"role": "kid"`). Role changes go in
the [trail](#the-trail) and can be undone.

## Three kinds of sprig

**Solo sprigs** are one person's: a comics tracker, a homelab helper. Set
`belongs_to` to their user id. Nobody else can see or talk to them.

**Family sprigs** are the household's: a meal planner, a chores keeper.
Every adult talks to them, and they keep **family memory**: what they learn
about the household ("we're vegetarian", "bins go out on Thursday") with
`remember` scope `family`. Family memory is separate from everyone's
private memory: a family sprig talking to Kim sees family memory and
Kim's own memories, never Sam's. Family sprigs put things on the shared
household calendar.

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
allow: []        # family_memory and/or calendar, if a parent allows
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
- **Kids talk only to their kid sprigs.** Naming Juni or a family sprig
  doesn't reach it, and adults can't chat with a kid sprig.

## Parents

In the web app, each kid sprig you set up appears under Household in the
sidebar ("Bramble’s chats"). It shows the whole chat, read-only, and what
the sprig may use: approve or remove tools one by one, and switch family
memory or the calendar on or off. Every change goes in the trail.

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
sprig put on their own calendar. Unlike the activity log, the trail is
never pruned.
