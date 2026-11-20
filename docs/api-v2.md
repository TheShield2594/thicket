# API v2: for apps

`/api/v2` is the versioned API apps use to connect to a self-hosted
Thicket: the iOS app first. It covers chat, the calendar, the household
and approvals. Everything follows the same rules as chat and the web app:
kids see only their kid sprigs, only parents read kid chats, only admins
change roles and levels, and changes go in the trail.

`GET /api/v2` tells an app what it's talking to:
`{"name": "thicket", "api": "2.0", "auth": ["password", "link"], "oidc": false}`.

## Signing in

`POST /api/v2/auth/login` with `{"username": "kim", "password": "…", "device": "Kim's iPhone"}`,
or `{"link": "<a one-time sign-in link or its token>", "device": "…"}`, returns
`{"token": "thk_api_…", "token_id": 12, "user": {…}}`. Send it as
`Authorization: Bearer thk_api_…` on every call. Wrong passwords lock a
username for 15 minutes after five tries, as in the web app.

`POST /api/v2/auth/logout` revokes the token. `cli` tokens work here too; an agent's `mcp` token doesn't ([kinds](api.md#tokens)). Admins see and revoke tokens
with `thicket api-token list` / `revoke`.

## Chat

The same threads as the web app: `<user id>.<sprig id>` for a sprig's own
thread, `<user id>.all` for the room where sprigs are named.

| | |
|---|---|
| `GET /api/v2/me` | the user, the sprigs they can talk to, kid sprigs they're a parent of |
| `GET /api/v2/chats/{chat}/messages?before=ID` | history, newest 60 |
| `POST /api/v2/chats/{chat}/messages` | `{"text": "…"}`, or a multipart form with `text` and `file` parts (images, PDFs, voice messages) |
| `DELETE /api/v2/chats/{chat}/messages/{id}` | delete (not in kids' chats) |
| `POST /api/v2/chats/{chat}/messages/{id}/pin` | `{"pinned": true}` |
| `GET /api/v2/chats/{chat}/messages/{id}/audio` | a spoken reply's audio, for messages with `"audio": true` |
| `PUT /api/v2/sprigs/{id}/voice` | `{"mode": "brief" \| "full" \| "off"}`: how this sprig speaks to you |
| `GET /api/v2/events` | server-sent events: new messages, typing, deletions, `audio` (a spoken reply was added to message `id`) |

Replies arrive on the event stream (or poll the history). Each sprig in
`/me` has `speaks` (`never`, `when_spoken_to`, `always`) and `voice_mode`
(`brief`, `full`, `off`).

## The weekly digest

The signed-in person's own choice of the [weekly digest](household.md#the-weekly-digest).
Nobody reads or changes anyone else's; kids get a 403 (they get no digest).

| | |
|---|---|
| `GET /api/v2/digest` | `{"mode": "always" \| "skip_empty" \| "off", "modes": [...], "day": "sunday" \| … \| "off", "time": "18:00", "timezone": "Europe/London"}`. `day` and `time` are the household's (`digest.day`, `digest.time`), in the person's own timezone; `day` `"off"` means the household has it switched off |
| `PUT /api/v2/digest` | `{"mode": "skip_empty"}`: `always` (a quiet week gets one line), `skip_empty` (a quiet week sends nothing) or `off`. Returns the same as `GET` |

The web app uses the same endpoints under `/api/`.

## Approvals and the trail

| | |
|---|---|
| `GET /api/v2/approvals` | what's waiting for this person's OK (`always_asks` says why one asks whatever the level, if it does) |
| `POST /api/v2/approvals/{id}` | `{"approve": true}` or `false` |
| `GET /api/v2/trail` | permission changes, sprig calendar entries and what sprigs did (admins: all) |
| `POST /api/v2/trail/{id}/undo` | undo one |
| `PUT /api/v2/levels` | admins: `{"sprig": "hal", "tool": "mcp_ha__turn_on", "level": "ask"}` (`""` resets) |
| `GET /api/v2/sprigs/{id}/mail/allowlist` | a sprig's mail allowlist, for whoever may read its mail: `{"allowlist": ["shop@vendor.example", "@vendor.example"]}` |
| `PATCH /api/v2/sprigs/{id}/mail/allowlist` | its owner only: `{"add": [...], "remove": [...]}`; in the trail. See [Email](mail.md) |

## Calendar

| | |
|---|---|
| `GET /api/v2/calendar` | `from`, `to` (dates or RFC 3339; default the next 7 days), `layers=events,jobs,outputs`, `sprigs=a,b` (only these), `hide=c` |
| `GET /api/v2/calendars` | connected calendars (own and shared) |
| `POST /api/v2/calendars` | connect one: see [Calendar](calendar.md) |
| `DELETE /api/v2/calendars/{id}` | disconnect one of yours |
| `DELETE /api/v2/calendar/outputs/{event_id}` | take a sprig output off the calendar: your own, or a shared one (adults). Items the person may remove have `"removable": true`. Returns `{"title", "trail_id"}`; undo the trail entry to put it back |

## Memory

What sprigs remember about the signed-in person, **facts** apart from
**relationship** memory (how things are going with one sprig: `tone`,
`likes`, `dislikes`, `inside_joke`, `shared_history`). Sprigs use a
change from their next reply.

| | |
|---|---|
| `GET /api/v2/memories` | `{"facts": [...], "relationship": [...]}`. `?sprig=juniper` narrows to the shared layer and that sprig's own; for a kid sprig you set up, `?sprig=bramble` shows what it remembers about its kids (403 for anyone else) |
| `POST /api/v2/memories` | `{"sprig": "juniper", "kind": "tone", "content": "Prefers short answers, no emoji."}`. `sprig` `""` is shared with every sprig (facts only); relationship kinds need a sprig. Parents add to a kid sprig's with its id (and `"person": KID_ID` if it has several kids). Locked unless `"locked": false`; `followup` isn't offered |
| `PATCH /api/v2/memories/{id}` | `{"content": "…", "kind": "likes"}`: locks it too unless `"locked": false` |
| `PUT /api/v2/memories/{id}/lock` | `{"locked": true}`: the memory review never changes a locked memory |
| `POST /api/v2/memories/{id}/undo` | undo the newest change made here (an edit, a lock, or adding it); again to go further back. `{"memory": null}` when it undid the add |
| `DELETE /api/v2/memories/{id}` | forget it for good, history and all: not undoable |

Each memory is `{"id", "group": "facts" \| "relationship", "kind",
"content", "scope": "shared" \| "sprig", "sprig", "sprig_name", "person",
"person_name" (when it isn't yours), "locked", "from" (who it came from,
in words: "memory review", "Juni, in chat", "you", a name), "origin",
"due_at", "created_at", "updated_at", "undoable"}`. Someone else's memory
is a 404, as if it didn't exist. Family memory isn't here: it changes
through family sprigs. The web app uses the same endpoints under `/api/`.

## Household

| | |
|---|---|
| `GET /api/v2/household` | members and their roles; sprigs and their tiers. Someone else's solo sprig is listed for admins only as `{"tier": "solo", "private": true, "belongs_to": 3}` |
| `POST /api/v2/household/invites` | admins: `{"name": "Ana", "role": "admin" \| "member" \| "kid", "own_sprig": true}` returns an invite link (7 days). `own_sprig` (adults only): Juni helps them make a private sprig of their own when they arrive |
| `PATCH /api/v2/household/members/{id}` | admins: `{"role": "kid"}` |
| `POST /api/v2/kid-sprigs` | admins: `{"id": "bramble", "name": "Bramble", "role": "A friendly companion for Max.", "kids": [7], "parents": [1, 2]}` |
| `GET /api/v2/kid-sprigs/{id}/chats?before=ID` | parents: the whole chat, newest first: `{"id", "role": "user" (the kid) or "assistant", "sprig", "kid_id", "content", "at"}` |
| `PATCH /api/v2/kid-sprigs/{id}/access` | parents: `{"add_tools": ["mcp:fortnite/player_stats"], "add_allow": ["calendar"], "remove_tools": [], "remove_allow": []}` |
| `PUT /api/v2/kid-sprigs/{id}/limits` | parents: time limits, replacing what's there: `{"hours": "07:00-20:00", "school_nights": "20:00", "daily_minutes": 30, "daily_messages": 0}` (empty or 0 = no limit). See [Time limits](household.md#time-limits) |

Errors are `{"error": "…"}` with a 4xx status. `/api/v1` (scripts and CI)
is unchanged; see [Local API](api.md).

## Reaching it from a phone

The API is on Thicket's web port. A phone needs to reach it: put Thicket
behind a reverse proxy with HTTPS (and set `THICKET_PUBLIC_URL`), or use a
VPN such as Tailscale. Expose `/api/v2/` only over HTTPS.
