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
account added later), admins included: only the owner gets the full setup tour. The family sprigs she introduces
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
anyone else's. A level set for every sprig is household policy for
shared sprigs: it doesn't reach solo sprigs, not even for core tools like
memory, so it can't change what someone's private sprig may do (yours
included: set your own solo sprig's levels on the sprig itself). Every refused attempt goes in the [trail](#the-trail) as
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

Admins create them by asking Juni, through `POST /api/v2/kid-sprigs`, or
by writing the file. In chat, Juni asks which kids it's for, who its
parents are, and a name and personality (`create_kid_sprig`); the new
sprig says hello in the chat as it would to the kid, and then the admin
sees a parent preview (who it talks to, who its parents are, that it
starts with no tools or adult data, its spend cap) and approves it.
Nothing is created before that. Afterwards its parents are told what
they can do (read its chats, approve tools one at a time, allow family
memory or the calendar, and that safety alerts come to them in chat),
and the kid gets a first hello from it. Their parents then decide what
it may use (in the web app, or `PATCH /api/v2/kid-sprigs/bramble/access`).
A kid sprig whose file was written by hand can't be changed by its
parents afterwards (Thicket never overwrites a hand-edited file), so
prefer Juni or the API.
Two ready-made kid sprigs come with Thicket, a homework helper and a
reading buddy: ask Juni, or `thicket sprig starter reading-buddy --kid
NAME`. The admin who adds one becomes its parent and sees everything it
will be first ([Starter sprigs](authoring.md#starter-sprigs)).

## What is enforced

These are enforced in code, whatever the model or a character file says.
Each claim names the test that proves it; CI runs them all on every
change.

- **Chat only, locked to read.** Any tool that changes something is
  refused, whatever permission level is stored, and levels for a kid
  sprig can't be raised. Nothing asks for approval.
  [`TestKidSprigBoundaries`](../internal/app/household_test.go)
- **Only tools a parent approved,** one at a time: `web`, or a single
  integration tool such as `mcp:fortnite/player_stats`. Whole
  integrations (`mcp:gmail/*`) are refused, and even an approved tool is
  only offered if it only reads. [`TestKidSprigBoundaries`](../internal/app/household_test.go),
  [`TestKidSprigValidation`](../internal/character/household_test.go)
- **No jobs, routines, triggers, webhooks, background tasks, follow-ups,
  check-ins or briefs.** A job owned by a kid sprig refuses to run.
  [`TestKidSprigBoundaries`](../internal/app/household_test.go), [`TestKidSprigHasNoPresets`](../internal/app/checkins_test.go)
- **No adult data** (calendars, email, family memory) unless a parent
  allows it in `allow`. With `calendar` allowed they see only shared
  household events, never a parent's own calendar.
  [`TestFamilyMemoryIsSeparate`](../internal/app/household_test.go), [`TestKidSprigBoundaries`](../internal/app/household_test.go),
  [`TestMCPCalendarSource`](../internal/app/calendar_test.go)
- **No talking out loud** unless a parent allows `voice`: then the kid
  sprig answers a voice message with one, like any sprig.
  [`TestKidSprigSpeaksOnlyWithParentsSayso`](../internal/app/speech_test.go)
- **Kids talk only to their kid sprigs.** Naming Juni or a family sprig
  doesn't reach it, and adults can't chat with a kid sprig.
  [`TestKidSprigBoundaries`](../internal/app/household_test.go)
- **Only its parents change what it may use, or its limits,** and every
  change goes in the trail and can be undone; a kid can change nothing.
  [`TestTrailUndo`](../internal/app/household_test.go), [`TestKidTimeLimits`](../internal/app/kidsafety_test.go)
- **Time limits** (below): at a limit the kid sprig says so kindly and
  stops, without calling the model; a message about self-harm is
  answered anyway. [`TestKidTimeLimits`](../internal/app/kidsafety_test.go),
  [`TestKidLimitsClosed`](../internal/character/household_test.go)
- **A spend cap only its parents can raise.**
  [`TestKidSprigMonthlyCap`](../internal/app/costs_test.go), [`TestNewKidSprigStartsCapped`](../internal/app/costs_test.go)
- **Personal details a kid shares stay in** (the containment test, below,
  and its limits).
  [`TestKidContainment`](../internal/app/kidsafety_test.go)

### Personal details stay in

What a kid tells their kid sprig is meant to stay with the kid, and with
their parents, who can read the whole chat. `TestKidContainment` checks
it: Max tells Bramble his full name, his address, his school, a
phone number and an email address, with every way out a parent can open
switched on (the web, an integration tool, family memory). Bramble then
tries to search the web for his school, open a map of his street and
call an integration with his name. The test asserts that none of those
details appear:

- in any adult's private memory or in family memory, after memory
  extraction runs on the kid's conversation (a kid's conversation never
  adds to family memory; what's remembered is the kid's own:
  [`TestReviewNeverWritesFamilyMemoryFromKids`](../internal/memory/review_test.go),
  [`TestKidRemembersPrivately`](../internal/app/remember_test.go));
- in any adult's chat, in any channel, or an adult's conversation
  summary, except the safety alert that goes to his parents;
- in a job, an inbox item, the activity log, or the trail (outside the
  kid's own safety entries, which only his parents see);
- in the arguments of any outbound call: web search queries, fetched
  page addresses, integration calls;
- in the topics of his parents' report.

The outbound calls are guarded in code: before a kid sprig sends a web
search, a page address or an integration call, its arguments are
checked for anything that looks personal (an email address, a phone
number, a street address) and for the details the kid shared in the
conversation or that the sprig remembers about them (their full name,
where they live, their school). A call that carries one isn't sent, the
sprig is told to try without it, and it's noted in the activity log
(without the details). Each argument is checked on its own and also
URL-decoded, and pinned messages count as things the kid said. Calls
without personal details go out as usual.

The check works on patterns. It catches the details it recognises, said
the ways it knows ("I live in…", "my name is…", "I go to…", a street
address, a school's name, a phone number or email address); a detail
phrased some other way can get past it. The test proves the paths above
for the details it uses, not that nothing could ever leak.
[`TestPersonalDetails`](../internal/agent/kidprivacy_test.go)

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
- **No alerts about ordinary chats.** Parents get a report instead (below), daily or weekly, as each chooses.
- **Safety alerts.** If a kid's message touches self-harm, a stranger
  asking for personal details (where they live, their school, photos, to
  meet), signs of abuse or grooming, or bullying, each parent gets a
  message from Juni with what was said. A cheap model screens messages
  that contain a warning word; messages about self-harm alert even if
  that check fails. Alerts are at most one an hour per kid and concern, go
  in the trail, and never block the chat.

## Time limits

A kid sprig's parents decide when their kid may chat with it and how
much, in the kid's timezone:

- **Chat hours** (`hours: 07:00-20:00`): outside them the sprig says
  it's not chat time and when it'll be back.
- **School nights** (`school_nights: 20:00`): no chatting from then
  until morning on Sunday to Thursday evenings. An evening time (17:00
  to 23:59), or one after midnight (00:00 to 04:59).
- **A daily allowance:** `daily_minutes` (active minutes: from the first
  message of a sitting to the last, where a pause of more than five
  minutes starts a new sitting) or `daily_messages` (the kid's messages
  it answered), counted from midnight.

At a limit the sprig says so kindly, in a fixed line ("That's all our
chatting for today! See you tomorrow!"), and no reply model is called.
The kid's message and that line are kept, so parents see them; they
don't count towards the allowance. Each stop is also noted in the trail
(at most once an hour per kid and limit). A kid sprig past its limits
doesn't pick up a handoff from another kid sprig either.

The safety check runs on every message either way. A message about
self-harm is always answered. A message with a warning word is screened
before the limit line goes out: if the safety check flags it, or can't
be reached, the kid gets a real answer instead. Attachments aren't read
past a limit, except voice notes, which are still transcribed so the
safety check hears what was said. If the day's count can't be read, the
sprig says it can't check chat time right now rather than assuming
there's time left.

Parents set them by asking Juni ("no Bramble after 8 on school nights,
and 30 minutes a day": `kid_limits`), on the kid sprig's page in the web
app, or with `PUT /api/v2/kid-sprigs/bramble/limits`
(`{"hours": "07:00-20:00", "school_nights": "20:00", "daily_minutes": 30, "daily_messages": 0}`).
They're kept in the character file:

```yaml
limits:
  hours: "07:00-20:00"
  school_nights: "20:00"
  daily_minutes: 30
```

Only its parents can change them; every change goes in the trail and
can be undone. Kids never reach Juni or anything that changes a sprig.

## Reports for parents

Each parent chooses how they hear about the kid sprigs they look after
(`kid_limits` with `report`, Settings in the web app, or
`PUT /api/digest` with `{"kids": "daily"}`):

- **weekly** (the default): in the weekly digest, the kid sprig's
  section adds time spent and the topics of the week;
- **daily**: a message from Juni each day at `digest.time`, per kid
  sprig, for the calendar day before (midnight to midnight, in your
  timezone): time spent, topics in a sentence or two,
  anything the safety check flagged, and what it cost. Nothing is sent
  for a day without chatting or alerts;
- **off**: no report (safety alerts still come at the time).

Topics are written by the cheap model from the kid's messages, told to
leave out names, schools, addresses and other personal details, and then
anything personal the kid shared is taken out in code. The cost is the
kid sprig's. [`TestKidReports`](../internal/app/kidsafety_test.go)

## The weekly digest

Once a week Juni sends each adult one short message about their week:
for each sprig they can see, what it did (job runs, replies, what it put
on the calendar or changed, from the trail), what's waiting on them
(open inbox items and approvals), and what it cost that week against its
cap, with a total and a link to the dashboard for the rest.

- **When:** `digest.day` (default `sunday`) at `digest.time` (default
  `18:00`), in each person's own timezone. `digest.day off` stops it for
  the household. It waits for the end of Juni's quiet hours, sends
  nothing while Thicket or Juni is paused, and is not limited by Juni's
  daily message budget (nor does it count against it). A digest that
  can't go out within a day of its time is skipped for that week.
- **What each person sees:** their own solo sprigs and the family
  sprigs; parents also get the kid sprigs they set up (replies to the
  kids, safety alerts). Never another adult's solo sprig, not even its
  name or what it cost. Job runs, inbox items and approvals are the
  person's own; a family sprig's changes are the person's own, or
  everyone's for admins, as under **Changes** on the dashboard. Kids get
  no digest.
- **Cost**, shown as the dashboard shows it (only admins see what a
  sprig costs in all):
  - a family sprig: admins see its whole week, for everyone it worked
    for, against its week; everyone else sees only **their share** (the
    spend recorded for them), never the family total or its cap;
  - your own solo sprig, and a kid sprig you're a parent of: its whole
    week against its week.

  There is no weekly cap setting: a sprig's week is seven times its
  daily cap (`budget.character.<id>.daily_usd`), and the digest says so:
  "$0.30 of a $7.00 week (7 × $1.00 daily cap)", or "no cap of its own".
  The total is the sum of the amounts that person was shown.
- **Quiet weeks** get one line. Tell Juni "skip the digest when there's
  nothing in it" (or "turn my weekly digest off", or "show me my
  digest") to change that for you; it's the `weekly_digest` tool. The
  same choice (every week, only weeks with something in it, or off) is
  under **Weekly digest** in your account in the web app, and at
  `/api/v2/digest` for apps. Each person sets only their own.
- It is put together from the database in code, not written by a model,
  so the numbers and the needs-you list are exactly what the records say.

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
