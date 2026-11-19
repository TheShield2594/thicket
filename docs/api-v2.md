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

`POST /api/v2/auth/logout` revokes the token. Admins see and revoke tokens
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
| `GET /api/v2/events` | server-sent events: new messages, typing, deletions |

Replies arrive on the event stream (or poll the history).

## Approvals and the trail

| | |
|---|---|
| `GET /api/v2/approvals` | what's waiting for this person's OK |
| `POST /api/v2/approvals/{id}` | `{"approve": true}` or `false` |
| `GET /api/v2/trail` | permission changes and sprig calendar entries (admins: all) |
| `POST /api/v2/trail/{id}/undo` | undo one |
| `PUT /api/v2/levels` | admins: `{"sprig": "hal", "tool": "mcp_ha__turn_on", "level": "ask"}` (`""` resets) |

## Calendar

| | |
|---|---|
| `GET /api/v2/calendar` | `from`, `to` (dates or RFC 3339; default the next 7 days), `layers=events,jobs,outputs`, `sprigs=a,b` (only these), `hide=c` |
| `GET /api/v2/calendars` | connected calendars (own and shared) |
| `POST /api/v2/calendars` | connect one: see [Calendar](calendar.md) |
| `DELETE /api/v2/calendars/{id}` | disconnect one of yours |

## Household

| | |
|---|---|
| `GET /api/v2/household` | members and their roles; sprigs and their tiers |
| `POST /api/v2/household/invites` | admins: `{"name": "Ana", "role": "admin" \| "member" \| "kid"}` returns an invite link (7 days) |
| `PATCH /api/v2/household/members/{id}` | admins: `{"role": "kid"}` |
| `POST /api/v2/kid-sprigs` | admins: `{"id": "bramble", "name": "Bramble", "role": "A friendly companion for Max.", "kids": [7], "parents": [1, 2]}` |
| `GET /api/v2/kid-sprigs/{id}/chats?before=ID` | parents: the whole chat, newest first |
| `PATCH /api/v2/kid-sprigs/{id}/access` | parents: `{"add_tools": ["mcp:fortnite/player_stats"], "add_allow": ["calendar"], "remove_tools": [], "remove_allow": []}` |

Errors are `{"error": "…"}` with a 4xx status. `/api/v1` (scripts and CI)
is unchanged; see [Local API](api.md).

## Reaching it from a phone

The API is on Thicket's web port. A phone needs to reach it: put Thicket
behind a reverse proxy with HTTPS (and set `THICKET_PUBLIC_URL`), or use a
VPN such as Tailscale. Expose `/api/v2/` only over HTTPS.
