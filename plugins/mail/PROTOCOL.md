# thicket-mail: the contract between Thicket and the mail plugin

Thicket core and this plugin talk over two channels, both on the plugin's
one port (default `:7072`), both needing `Authorization: Bearer <token>`
(the shared token file, read on every request, compared in constant time;
no file or a token under 32 characters refuses everything). Only
`GET /health` answers without a token.

1. **MCP** at `POST /mcp` (Streamable HTTP, plain JSON answers): the tools
   sprigs call.
2. **Admin API** under `/v1/`: what core alone uses (accounts, events, the
   dashboard's mailbox view, a person's send/discard). Sprigs never reach
   it: they only get MCP tools.

The plugin keeps credentials in memory only. Core holds them in its
encrypted secret store and pushes them (`PUT /v1/accounts`) on connect,
whenever they change, and every minute (idempotent). After a restart the
plugin has no accounts until core pushes them again.

## Who is calling: `_meta` on every `tools/call`

Core adds to the `tools/call` params:

```json
{"name": "send_mail", "arguments": {...},
 "_meta": {
   "thicket/call": {"sprig": "pip", "approved": false, "draft_only": false},
   "thicket/mail": {"allow": ["shop@vendor.example", "@vendor.example"]}
 }}
```

- `sprig`: the sprig whose mailbox this is. The plugin never takes the sprig
  from `arguments`. Missing or unknown sprig → tool error ("Pip has no mail
  address yet; its owner can set one up").
- `approved`: a person approved this exact call (an approval prompt, or
  "Do it" on a draft in "needs you"). Only then may a message go to a
  recipient off the allowlist.
- `draft_only`: the sprig is at draft level for this tool. Whatever the
  allowlist says, only draft.
- `allow`: the sprig's mail allowlist as it is now: exact addresses
  (lower-cased) and `@domain` entries. Matching is case-insensitive, exact
  on the address, and `@domain` matches that domain only (not subdomains).

## Tools

All take the mailbox from `_meta`, never from arguments.

| Tool | readOnlyHint | `_meta` on the tool (tools/list) |
|---|---|---|
| `list_mail` {folder: inbox\|drafts\|sent\|archive (default inbox), limit 1–50 (default 20), unread_only} | true | |
| `read_mail` {id} | true | |
| `draft_mail` {to[], cc[], bcc[], subject, body, in_reply_to?} | false | `thicket/gates_itself: true`, `thicket/trailed: true` |
| `send_mail` {to[], cc[], bcc[], subject, body} | false | `thicket/gates_itself: true`, `thicket/trailed: true` |
| `reply_mail` {id, body, reply_all?} | false | `thicket/gates_itself: true`, `thicket/trailed: true` |
| `send_draft` {draft_id} | false | `thicket/gates_itself: true`, `thicket/trailed: true` |
| `discard_draft` {draft_id} | false | `thicket/trailed: true` |
| `archive_mail` {id} | false | `thicket/trailed: true` |

`thicket/gates_itself` tells core the tool enforces its own sending rules:
core calls it at `act` and `draft` levels (with `draft_only` at draft),
asks first at `ask`, refuses at `read`, and its default level is `draft`.
`thicket/trailed` tells core the plugin's events trail it, so core doesn't
add its own trail entry with the arguments (bodies stay out of the trail).

### Sending rules (in code)

For `send_mail`, `reply_mail` and `send_draft`, the message is **sent** only
when:

- `approved` is true; or
- `draft_only` is false, **every** recipient (to, cc, bcc) is on `allow`,
  and the sprig has sent fewer than its daily limit (rolling 24 hours,
  persisted across restarts; approved sends count but are never refused).

Otherwise it is saved to the mailbox's Drafts (JMAP: the Drafts mailbox,
`$draft` keyword; IMAP: APPEND to the Drafts folder with `\Draft`) and the
result says so, naming the first recipient that wasn't allowed (or the
limit, or draft level), and carries:

```json
{"_meta": {"thicket/drafted": {"draft_id": "…", "from": "pip@…", "to": [...], "cc": [...], "bcc": [...],
  "subject": "…", "preview": "first 500 characters of the body", "reason": "jane@x.example isn't on Pip's mail allowlist"}}}
```

Core turns that into a "needs you" item whose **Do it** calls `send_draft`
with `approved: true` and whose dismissal calls the admin API's discard.
`draft_mail` always drafts and carries the same `_meta`. A `send_draft` that
still can't send (not approved, off the list) drafts nothing new and
carries no `thicket/drafted`: it says the draft is waiting in "needs you".

Replying to someone who wrote first does not add them to `allow`.
`reply_mail` sends to the original's `Reply-To` (else `From`); with
`reply_all`, also its To and Cc, minus the sprig's own address. It sets
`In-Reply-To` and `References`, and prefixes "Re: " once.

### Untrusted content and caps

- Everything returned is outside content; core wraps it.
- HTML is converted to text in the plugin: no markup, no remote images
  (`[image: alt]`), links written as `text <https://real.target>`; scripts
  and styles dropped entirely.
- Caps: body text 32 KiB to the sprig (cut with a note), at most 20
  attachments listed (name, type, size; never content), 20 recipients per
  message, 64 KiB body when composing, subject 300 characters.
- Mail older than the account's `retention_days` is not listed or read for
  the sprig (the provider keeps it; the dashboard still shows it), nor sent
  with an unapproved `send_draft` (a person's approval of that draft lifts it).
- A received message that looks like a one-time code (a 4–8 digit code near
  "code", "verification", "one-time", "OTP", "2FA", "security code",
  "passcode") has the code replaced in `read_mail` with
  `[one-time code hidden: shown to the person in "needs you"]`, and its
  `received` event has `"otp": true`.

## Admin API

### `PUT /v1/accounts`

Replaces the whole set. Unchanged accounts keep their connection.

```json
{"accounts": [{
  "sprig": "pip", "address": "pip@example.org", "name": "Pip",
  "backend": "jmap",
  "jmap": {"session_url": "https://api.fastmail.com/jmap/session"},
  "imap": {"host": "imap.example.org", "port": 993, "security": "tls"},
  "smtp": {"host": "smtp.example.org", "port": 465, "security": "tls"},
  "username": "pip@example.org", "password": "app-password",
  "retention_days": 90, "send_limit": 20
}]}
```

`backend` is `jmap` or `imap`. JMAP uses Basic auth with username and
password, or `Authorization: Bearer <password>` when username is empty.
IMAP `security` is `tls` (implicit) or `starttls`; SMTP `tls` (implicit,
465) or `starttls` (587). Anything else, or a server that doesn't offer
STARTTLS, is refused: never plaintext. Answer:
`{"accounts": [{"sprig": "pip", "state": "connected"|"connecting"|"error", "error": "…"}]}`.

### `GET /v1/events?after=SEQ&wait=SECONDS`

Long poll (wait ≤ 30). Answer:

```json
{"epoch": "random per plugin start", "next": 42, "events": [
  {"seq": 41, "sprig": "pip", "kind": "received", "at": "RFC3339",
   "from": "a@x", "to": ["pip@…"], "cc": [], "subject": "…", "size": 1234,
   "message_id": "<…>", "otp": false},
  {"seq": 42, "sprig": "pip", "kind": "sent", "approved": true, …},
  {"kind": "drafted", "draft_id": "…", …}, {"kind": "discarded", …}, {"kind": "archived", …},
  {"kind": "error", "detail": "login failed"}
]}
```

Kinds: `received`, `sent`, `drafted`, `discarded`, `archived`, `error`.
Bodies are never in events. Mail already in the inbox when an account first
connects is not an event (only mail that arrives after). The plugin keeps
the last 1000 events in memory; when `epoch` changes, core starts again
from `after=0`.

### Mailbox (for the dashboard)

- `GET /v1/mailbox/{sprig}?folder=inbox|drafts&limit=N` →
  `{"messages": [{"id","from","to":[],"subject","date","size","preview","attachments":N,"unread":bool,"draft":bool}]}`
  newest first, no retention filter.
- `GET /v1/mailbox/{sprig}/messages/{id}` →
  `{"id","from","to":[],"cc":[],"bcc":[],"subject","date","size","text","draft":bool,"attachments":[{"id","name","type","size"}]}`.
  `text` is the same text conversion as `read_mail` (links as
  `text <url>`, no images), but one-time codes are shown (this is the
  person).
- `GET /v1/mailbox/{sprig}/messages/{id}/attachments/{aid}` → the bytes,
  `Content-Type: application/octet-stream`, headers `X-Mail-Filename`,
  `X-Mail-Type`.
- `POST /v1/mailbox/{sprig}/drafts/{id}/send` → a person sent it:
  `{"message_id": "…"}` (event `sent`, `approved: true`).
- `POST /v1/mailbox/{sprig}/drafts/{id}/discard` → `{}` (event `discarded`).

Errors are `{"error": "…"}` with 4xx/5xx. Under `/v1/mailbox/{sprig}`:

- **503**: the account is unavailable: not connected right now, or no
  account for that sprig at all (unknown, or not pushed yet after a plugin
  restart). Nothing is known about the draft or message; try again later.
- **404**: the account is connected and the message, draft or attachment
  doesn't exist there (or isn't a draft, for send/discard, or the draft
  was already sent).
- **409**: the draft is being sent right now by another call.
- **400**: the draft can't be sent as it is (no recipients, too many).
- **502**: the mail server failed or refused. When the server took a
  message but never confirmed it, the error says so; that draft is not
  sent again (a later send answers 404).

### `GET /v1/doctor`

```json
{"accounts": [{"sprig": "pip", "address": "pip@…", "checks": [
  {"name": "session", "ok": true, "detail": "…"},
  {"name": "auth", "ok": true},
  {"name": "push", "ok": true, "detail": "EventSource" | "polling every 60s" | "IDLE"},
  {"name": "smtp", "ok": true},
  {"name": "dns", "ok": true, "warn": true, "detail": "no DMARC record for example.org"}
]}]}
```

`warn: true` is advice only (SPF/DKIM/DMARC for the sending domain).

## Details the implementation settled

- `send_limit` 0 or missing means 20; `retention_days` 0 or missing, 90.
- `list_mail` and `read_mail` answer with indented JSON as their text;
  `read_mail` adds `reply_to` and `more_attachments` when relevant and
  marks the message read.
- `draft_mail`'s `in_reply_to` is a message id from `list_mail` or a
  literal `<Message-ID>`.
- Events may carry `bcc`, `reason` and `size` (drafted), `id` (archived);
  empty arrays are left out.
- The mailbox listing also takes `folder=sent|archive`; `limit` defaults
  to 50, at most 200.
- A one-time code is hidden in the subject too, for the sprig, and in a
  reply's "Re:" subject. `list_mail` previews are made from the masked text;
  where only the provider's own preview is known, every number that could be
  a code is shown as `[number hidden]`.
- `send_draft` and the dashboard's send submit the draft as it is in Drafts
  (IMAP: its original MIME, attachments included, with the Bcc header
  removed and the Bcc recipients in the envelope only). A draft is sent at
  most once: a second send of the same draft, at the same time or after it
  went out, is refused. A submission the server took but never confirmed
  is reported as an error saying it may have been sent; it still counts
  toward the daily limit and is not sent again automatically.
- When the daily send count can't be saved, unapproved sends are drafted
  instead (approved ones still go).
