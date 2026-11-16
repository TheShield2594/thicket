# Event triggers and the commons

## Routines that fire on events

Besides a schedule, a routine can run when something happens. Thicket has
no integrations of its own, so events come from what's plugged in:

**An integration that changed.** Ask a sprig: "When I get an email from
Maya, summarise it for me." It sets up a trigger (`create_trigger`) that
calls one of an integration's tools every few minutes, for example a mail
search, a calendar listing or a container status, and runs the routine
with whatever is **new** since the last check. The first check only notes
what's already there. Only tools the integration marks as read-only can
be watched, and the sprig must already have access to that tool.

| Setting | |
|---|---|
| `every_minutes` | how often to check: 5 to 1440, default 15 |
| `match` | only new items containing this text (case-insensitive), or matching `/a regular expression/` |
| `args` | arguments for the tool, e.g. a search query |

Items are the elements of a JSON array the tool returns (or the first
array inside a JSON object), else lines of text. An item is new when it
wasn't in the last check, or when an item with the same `id` (or `uid`,
`message_id`...) has changed: a container that was healthy and is now
unhealthy fires. Up to 20 items go to each run; the rest come in the
next. The first 1,000 items of a response are considered.

Each check also confirms the sprig still has access to the tool and that
the tool still only reads; if not, the trigger holds and tells you.

Examples:

- new email from X: a mail integration's search tool with
  `{"query": "from:maya@example.com"}`;
- calendar event added: a calendar integration's list-events tool;
- container unhealthy: a Docker or Portainer integration's list tool,
  `match: unhealthy`.

A check that fails lands in your inbox (repeats count up on one item).
Polling itself costs nothing; a run costs what any job does and counts as
heartbeat spend. While a spend cap is reached, checks wait, so nothing
seen meanwhile is lost.

**Something that calls in.** A [webhook](webhooks.md) can take a `match`
filter too: calls whose body doesn't match are answered `200 ignored`
and don't run anything. Monitoring tools (Uptime Kuma, Healthchecks,
Portainer) can post to it, with `match: unhealthy` or
`/"status":\s*"down"/`.

`list_jobs` shows each routine's trigger and the last check's problem, if
any.

## The commons

Set `THICKET_COMMONS_CHANNEL` to a Discord channel id (or
`telegram:<chat id>`) to make it the commons: a shared channel where
sprigs answer every message and hand work to each other in the open. A
sprig that @mentions another in its reply ("@Pip, can you check Friday?")
hands the conversation over, and Pip replies next. You can @mention a
sprig to address it too.

Every handoff, here or anywhere, is logged to the trail (kind `handoff`),
as are questions and background tasks between sprigs. Chains stop after
two handoffs per message.
