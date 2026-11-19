# Calendar

The household calendar merges three layers, each of which can be shown or
hidden, with toggles per sprig:

| Layer | What | Who sees it |
|---|---|---|
| `events` | Calendars people connected, read-only | Each person sees their own, plus calendars marked shared |
| `jobs` | Sprig jobs: scheduled runs, and past runs that ran or failed | The person the jobs are for |
| `outputs` | Things sprigs put on the calendar (dinners from a meal planner, release dates), each linking back to its source | The person, plus shared entries for everyone |

Kids don't have the calendar. A kid sprig whose parent allowed `calendar`
sees shared household items only.

## In the web app

**Calendar**, under Household in the sidebar, shows a week at a time:
toggle the layers (events, sprig jobs, sprig outputs) and each sprig on or
off (the choices are remembered), step between weeks, and follow an
output's link back to its source. **Remove** takes a sprig output off the
calendar: one of your own, or a shared household one. It goes in the
[trail](household.md#the-trail), so undo it under **Changes** on the
dashboard to put it back. Below it, connect and disconnect your
calendars.

## Connecting a calendar

Adults connect calendars in the web app (Calendar → Connect a calendar)
or through the app API (`POST /api/v2/calendars`):

- **A calendar feed (ICS):** Google Calendar's "secret address in iCal
  format", iCloud's public calendar link, Outlook's published ICS link,
  or any `webcal://` link. `{"name": "Work", "kind": "ics", "url": "https://…"}`
- **CalDAV:** Nextcloud, Fastmail, iCloud (with an app-specific password),
  Radicale. Give the calendar's collection URL with a username and
  password. `{"name": "Family", "kind": "caldav", "url": "https://dav.example.com/calendars/kim/family/", "username": "kim", "password": "…", "shared": true}`

- **Through an integration (admins):** a calendar an MCP server reaches
  (a Google Calendar server, Composio, an Outlook server). Name the
  integration as connected and its read-only tool that lists events, and
  say what shape its result has (see below).
  `{"name": "Family", "kind": "mcp", "mcp": {"integration": "gcal", "tool": "list_events", "preset": "google", "args": {"calendarId": "family@group.calendar.google.com"}}, "shared": true}`

Thicket checks the calendar loads before saving it. The address (feed
links carry a private token) and the password go in the encrypted store,
never the database. Calendars are read-only: Thicket never writes to
them. Recurring events, time zones, all-day events, exceptions and
cancellations are handled; events are re-read at most every 5 minutes.

`"shared": true` puts it on every adult's calendar.

### Calendars through an integration

Only the owner and admins connect these, since they read through the
household's connection to the integration. Thicket calls the tool only
if the integration lists it as read-only (`readOnlyHint`), on every
call: a tool that changes anything is refused when you connect it and
never called afterwards. Results are cached for 5 minutes like the other
calendars, and the same rules apply: your own unless shared, and kids
(and kid sprigs) never see an adult's own calendar.

The tool's result must be JSON. How it maps to events:

- `preset`: `google` (Google Calendar's `events.list`: passes the range
  as `timeMin`/`timeMax`, reads `items[].summary`, `start`, `end`,
  `location`, `htmlLink`) or `outlook` (Microsoft Graph's
  `calendarView`: `startDateTime`/`endDateTime`, `value[].subject`, …).
- `args`: fixed arguments for every call, such as a calendar id. They
  win over the preset's.
- `from_arg`, `to_arg`: the arguments that take the start and end of the
  range (RFC 3339). Without them the range isn't passed, and events are
  filtered after the call.
- `map`: where each field is, as a dotted path, or several separated by
  `|` (the first that's there wins): `items` (the list of events),
  `title`, `start`, `end`, `all_day`, `location`, `link`, `id`. Fields
  left out use common names (`summary|title|subject|name`,
  `start|startTime|start_time|starts_at`, …), so many servers need no
  mapping at all.

A start or end can be an RFC 3339 time, a local date-time (read in your
timezone), a date (an all-day event), a Unix time, or an object like
`{"dateTime": …, "timeZone": …}` or `{"date": …}`. Results without a
title or a start are skipped; links other than http(s) are dropped.

## Sprigs and the calendar

Give a sprig the `calendar` scope and it can:

- `calendar`: read the next days (all layers, or some).
- `add_to_calendar`: put something on it, with a link back to where it
  came from. Family sprigs add to the shared household calendar; others
  to the person's own. Each entry goes in the [trail](household.md#the-trail)
  and can be undone.
- `remove_from_calendar`: take an entry off again (also undoable).

These act by default, like scheduling a job; set the sprig's level to
`ask` to approve each one, or `draft` to review them in the inbox.

## In the app

`GET /api/v2/calendar?from=2026-11-16&to=2026-11-23&layers=events,outputs&hide=hal`
returns the merged items with each sprig's name, avatar and colour.
`DELETE /api/v2/calendar/outputs/{event_id}` removes a sprig output (the
ones marked `"removable": true`). See [API v2](api-v2.md).
