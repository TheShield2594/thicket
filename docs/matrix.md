# Matrix

Thicket can talk on Matrix as well as Telegram, Discord and the web app.
It's off unless you turn it on. Like Telegram, it uses long polling
(`/sync`), so it needs no public address or open port.

## Setting up the bot

1. Make a Matrix account for the bot on your homeserver (Synapse,
   Conduit, Dendrite, or a hosted one), such as `@thicket:example.org`.
2. Sign in to it once and copy its access token. In Element: Settings,
   Help & About, **Access Token**. Then close that session without
   signing out (signing out ends the token).
3. Set `THICKET_MATRIX_HOMESERVER` to the homeserver's client-server
   address, such as `https://matrix.example.org`. Plain `http://` works
   only for a homeserver on this machine or your local network
   (localhost, a private IP address, or a Docker service name such as
   `http://synapse:8008`), because the token goes with every request.
4. Give Thicket the token: send it to Juni in a DM on the web app (a
   Synapse token starting `syt_` is spotted and connected straight away),
   or `thicket secrets set matrix_token`, or set `THICKET_MATRIX_TOKEN`
   (stored encrypted on first start; you can then remove it).
5. Start Thicket, then start a chat with the bot from your own Matrix
   account. It joins rooms it's invited to.

If Thicket has no owner yet, the first person to DM the bot becomes the
owner, unless `THICKET_OWNER_MATRIX_ID` (your full id,
`@you:example.org`) names someone. Without `THICKET_MATRIX_HOMESERVER`,
or without a token, Matrix stays off and everything else starts as
usual. `thicket check` says which, and `thicket doctor` asks the
homeserver whose token it is.

## Encrypted rooms aren't supported

Thicket can't read end-to-end encrypted rooms yet. It declines invites
to them, giving the reason (your client shows it with the declined
invite), and if encryption is turned on in a room it's in, it says why
and leaves. Make the room with encryption off from the start: in
Element's "New room" dialog, turn **Enable end-to-end encryption** off
before you create it. Encryption can't be turned off once a room has it,
so for a room that's already encrypted, make a new one. Element turns it
on by default for DMs, so start a DM by making a new room this way and
inviting the bot.

## How it looks

The bot posts under one name and picture, so each reply starts with the
speaking sprig's name in bold. Sprigs' Markdown is sent as Matrix
formatting (bold, code, links, headings); the plain text is there too
for clients that don't show formatting. Sprig replies never ping
anyone. Per-sprig names (changing the bot's room display name for each
reply) aren't used: they would flicker in every member list.

Images and files work as on Telegram, with the homeserver's own upload
size limit. Thicket uses the authenticated media API and falls back to
the older download API on homeservers without it. A caption sent with a
file becomes the message's text.

Messages sent while Thicket was offline aren't answered when it comes
back (no late replies to old questions); only new ones are.

"Delete that" redacts the sprig's message. When a message looks like a
key or password, Thicket redacts it too, if the bot is allowed to (it is
in a DM; in a room, it needs a power level that lets it redact others'
messages).

## Rooms

Invite the bot to a room, and sprigs answer when you mention the bot (a
pill, its name at the start of the message: "Thicket: what's on
today?"), reply to one of its messages, or address a sprig by name. List
room ids (`!abc123:example.org`, in Element under the room's settings,
Advanced) in `THICKET_MATRIX_ROOMS` to have sprigs answer every message
there. Room aliases (`#family:example.org`) aren't accepted there; use
the id.

## Using Matrix with the other apps

Linking works as on the other apps: on the app Thicket already knows you
on, ask Juni to link your Matrix account, then send the one-time code to
the bot in a DM on Matrix. "Reach me on Matrix" makes it where check-ins
and job results go.

To let someone else in, invite them by their full Matrix id ("invite
Matrix user @alice:example.org"), or make an invite for Matrix: they DM
the bot the `join-…` code it gives.

React with 🧠 (or ✍) to a message to have it remembered.

## Settings

| Variable | |
|---|---|
| `THICKET_MATRIX_HOMESERVER` | the homeserver's address, such as `https://matrix.example.org`. Matrix is off without it |
| `THICKET_MATRIX_TOKEN` | the bot account's access token (or `thicket secrets set matrix_token`) |
| `THICKET_OWNER_MATRIX_ID` | your Matrix id, `@you:example.org` (optional) |
| `THICKET_MATRIX_ROOMS` | comma-separated room ids where sprigs answer everything |

`THICKET_ALLOWED_USERS` accepts `matrix:@alice:example.org` as well.
`THICKET_COMMONS_CHANNEL` can be `matrix:!roomid:example.org`.
