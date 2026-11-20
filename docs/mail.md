# Email: a sprig's own address

A sprig can have its own email address for signups, confirmations,
receipts and newsletters, and to write to vendors for you. It's the
`thicket-mail` plugin: a container of its own that speaks **JMAP**
(Fastmail and others) or **IMAP and SMTP**, always over TLS. It's off
until you start it and give it to a sprig, and kid sprigs can't have it.

This is for *sprigs'* addresses. To let a sprig help with *your own*
mail, connect your provider's MCP server as an integration instead
([Integrations](mcp.md)).

## What you get

- Each sprig gets one address, an alias or a mailbox on a domain you own.
  You make it at your provider; Thicket never creates addresses (yet).
- New mail can start the sprig's routines, like any event ("when a
  receipt arrives, add it to the budget"). It counts as outside content
  from the start: it can't make the sprig do anything you didn't ask for.
- The sprig can list, read, draft, send, reply to and archive mail.
- **It only sends on its own to addresses you allowed.** Everything else
  is a draft, in its mailbox's Drafts and in "needs you", for you to send
  or discard.
- Every message sent or received is in the trail: from, to, subject,
  size and message id. Never the body.
- You can read each sprig's mailbox, and send or discard its drafts, on
  the [dashboard](dashboard.md#mail).

## Setting it up

1. **Make the address.** At your provider, create an alias or a mailbox
   for the sprig (`pip@yourdomain.example`) and an **app password** for
   it (Fastmail: Settings → Privacy & Security → App passwords, with
   access to Mail; for JMAP, an API token works too). Don't give Thicket
   your main password.
2. **Start the plugin.** `docker compose --profile mail up -d`. Thicket
   finds it at `http://mail:7072` (or `THICKET_MAIL_URL`) and shares a
   token with it through the `mail-auth` volume; `thicket doctor` shows
   `plugin.mail`. Thicket looks for a plugin at its default address only
   in the first minute after it starts, so if Thicket was already running,
   restart it after starting the plugin (`docker compose restart
   thicket`).
3. **Tell Juni**, in a DM: "give Pip the address pip@yourdomain.example on
   Fastmail". For JMAP Juni needs the session URL
   (`https://api.fastmail.com/jmap/session` for Fastmail); for IMAP, the
   IMAP and SMTP servers (`tls` on 993 and 465, or `starttls` on 143 and
   587). Then send the app password as your next message: Thicket
   captures it, deletes your message where it can, stores it encrypted
   and adds it to the redactor. The sprig and the model never see it.
4. **Give it to the sprig**: "give Pip email" grants `mcp:mail/*`.
5. **Allow who it may email on its own** (optional): "Pip may email
   orders@shop.example and anyone at @vendor.example". Until you do,
   every email it writes waits for you.

Only the sprig's owner can do steps 3–5: the person a private sprig
belongs to, or the owner or an admin for a shared sprig.

The address, its password and its allowlist can also be changed later
in chat ("show Pip's email settings", "remove Pip's address"). Removing
an address only stops Thicket using it; the mailbox stays at your
provider.

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `retention_days` | 90 | mail older than this isn't offered to the sprig (your provider keeps it; the dashboard still shows it) |
| `send_limit` | 20 | messages a day the sprig may send on its own (rolling 24 hours); messages you send for it count, but are never refused |

## When a sprig sends

`send_mail`, `reply_mail` and `send_draft` send straight away only when
all of these hold, checked in code by the plugin on every message:

- the sprig's level for the tool is `act` (see [Permission
  levels](mcp.md#approvals-and-permission-levels)), and nothing from outside
  content is driving it (an email can't make a sprig send an email);
- **every** recipient, to, cc and bcc, is on its allowlist (an exact
  address, or `@domain` for that domain only, not its subdomains);
- it's under its daily limit.

Otherwise the message is saved to the mailbox's Drafts and "needs you"
shows it with **Send** and **Discard**, saying why it wasn't sent ("jane@x
isn't on Pip's mail allowlist"). At `draft` level the sprig always drafts;
at `ask` it asks you first; at `read` it can only read. New sprigs start
at `draft`, so to let one send to its allowlist, set its mail tools to
`act` ("let Pip send email on its own").

Replying to someone who wrote first does **not** add them to the
allowlist. `draft_mail` always drafts. Approving a send yourself (from
"needs you", the approval prompt or the dashboard) sends it to anyone.

### Things a sprig never does with mail

- **Follow a link by itself.** Confirming a signup means opening the
  link with `fetch_page` or the browser, at that tool's own level, and
  only when you asked for it, never because an email said so.
- **Use a one-time code.** When a message looks like it carries a
  sign-in or verification code, the sprig sees the code hidden, and
  "needs you" tells you a code arrived. You read it in the mailbox and
  type it in yourself.
- **See a password.** Mail passwords are in Thicket's encrypted store and
  only in the plugin container's memory; they are never logged.

## What the sprig reads

Mail is outside content: it is wrapped as untrusted data and can't steer
what the sprig does. HTML is turned into text in the plugin before the
model sees it: no markup, scripts or styles, no remote images (`[image:
alt]`), and links written as `text <https://real.target>`. Attachments are
listed by name, type and size; their contents are never given to the
model. Caps: 32 KB of body text, 20 attachments listed, 20 recipients
and 64 KB per message written.

## Tools

| Tool | What it does | Level |
|---|---|---|
| `list_mail` | inbox, drafts, sent or archive, newest first | reads |
| `read_mail` | one message, as text | reads |
| `draft_mail` | saves a draft for you | always drafts |
| `send_mail` | sends, or drafts (see above) | gates itself |
| `reply_mail` | replies (or replies all), or drafts | gates itself |
| `send_draft` | sends a draft, or leaves it for you | gates itself |
| `discard_draft` | deletes a draft | asks by default |
| `archive_mail` | moves a message to Archive | asks by default |

A routine can fire on new mail: "when Pip gets an email from the bank,
tell me" makes a trigger on `list_mail`. New mail is checked at once
(push), not only on the trigger's interval.

## Your domain's DNS

For mail from your domain to be delivered rather than marked as spam,
your provider tells you which records to add. `thicket doctor` warns (it
doesn't fail) when one is missing:

- **SPF**: a TXT record on the domain listing who may send for it, e.g.
  `v=spf1 include:spf.messagingengine.com ~all` for Fastmail.
- **DKIM**: the signing key your provider gives you, as TXT or CNAME
  records under `<selector>._domainkey.yourdomain.example` (Fastmail:
  `fm1`, `fm2`, `fm3` CNAMEs).
- **DMARC**: a TXT record at `_dmarc.yourdomain.example`, e.g.
  `v=DMARC1; p=quarantine; rua=mailto:you@yourdomain.example`.
- **MX**: if the domain receives mail at this provider, its MX records.

## Checking it

`thicket doctor`'s `plugin.mail` line, per address: the server answers
(JMAP session, or IMAP and SMTP), the login works, new mail arrives
(JMAP push, IMAP IDLE, or polling every minute when neither is
available), SMTP login works (IMAP), and advice about SPF, DKIM and DMARC
for the sending domain.

## How it works

The plugin (`plugins/mail`, its own Go module) holds no passwords on disk:
Thicket pushes each address and its password to it when it connects and
every 30 seconds. Each call a sprig makes says, from Thicket's side,
which sprig it is, whether you approved it, whether it may only draft,
and its allowlist as it stands at that moment, so a removal counts at
once; the sprig's own arguments can't choose a mailbox. The plugin
reports what happens (received, sent) on an event stream Thicket follows
for the trail and triggers. The contract is
[`plugins/mail/PROTOCOL.md`](../plugins/mail/PROTOCOL.md).

Tests: `plugins/mail` against fake JMAP, IMAP and SMTP servers (including
servers that offer no TLS, which are refused), `internal/app/mail_test.go`
for Thicket's side (sending rules, allowlist ownership, kid sprigs, the
dashboard mailbox and injection through subject, sender and body).
