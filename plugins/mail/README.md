# thicket-mail

The mail plugin for Thicket: each sprig gets its own mailbox, over
**JMAP** or **IMAP + SMTP**, always with TLS. What it does for people is
in [docs/mail.md](../../docs/mail.md); the contract with Thicket core
(MCP tools, `_meta`, sending rules, caps, the `/v1/` admin API) is
[PROTOCOL.md](PROTOCOL.md).

```
docker build -t thicket-mail plugins/mail
cd plugins/mail && go vet ./... && go test -race ./...
```

| Env | Default | |
|---|---|---|
| `MAIL_ADDR` | `:7072` | listen address |
| `MAIL_TOKEN_FILE` | `/plugin-auth/token` | the token shared with Thicket, read on every request |
| `MAIL_STATE_DIR` | `/var/lib/thicket-mail` | the daily send counts (`sends.json`), nothing else |

## Layout

| File | |
|---|---|
| `main.go` | server, token check, routes, timing |
| `accounts.go` | `PUT /v1/accounts`, the connect/reconnect loop, the `Backend` interface |
| `mcp.go`, `tools.go` | the MCP tools and the sending rules |
| `admin.go` | the dashboard's mailbox view and `/v1/doctor` |
| `events.go` | the event ring (last 1000) and the `/v1/events` long poll |
| `sendlog.go` | the per-sprig daily send count, persisted atomically |
| `text.go`, `mime.go` | HTML to text, MIME, header cleaning, one-time codes |
| `jmap.go` | the JMAP backend |
| `imap.go`, `smtp.go` | the IMAP backend and SMTP submission |

## Security

- Credentials live in memory only: never written, never logged, and
  scrubbed from every error before it leaves the plugin.
- Every connection verifies the server's certificate (system roots).
  IMAP and SMTP use TLS from the first byte or STARTTLS; any other
  setting, or a server that doesn't offer STARTTLS, is refused before a
  password is sent. JMAP URLs must be https, redirects too.
- Mail is outside content. HTML is converted here: scripts, styles and
  other non-content dropped, images never fetched (`[image: alt]`),
  links written as `text <target>` with the real target only
  (`javascript:` and `data:` become `<link removed>`), control and bidi
  characters removed, display names stripped of anything that could
  pass for a second address.
- The sending rules (approval, draft level, allowlist, daily limit) are
  enforced here, in code, on every message.

## Why an IMAP library

JMAP is JSON over HTTPS, so its client is written by hand in `jmap.go`.
IMAP is different: a stateful line protocol with literals, untagged
responses arriving at any time, IDLE, and MIME bodies. Hand-rolling a
parser for that is error-prone, and in a component that handles
passwords and hostile input, security-sensitive. So the IMAP backend uses
[go-imap](https://github.com/emersion/go-imap) v1 (client, with IDLE and
MOVE) and [go-message](https://github.com/emersion/go-message) for MIME
and legacy charsets. SMTP submission uses Go's own `net/smtp`;
[go-smtp](https://github.com/emersion/go-smtp) is only a fake server in
the tests. These dependencies stay in this plugin's `go.mod`, not
Thicket's.

## Tests

The tests run against fakes: a JMAP server (`fakejmap_test.go`, with
EventSource), go-imap's memory server behind a lock, and a go-smtp
server (`fakeimap_test.go`), all over TLS with a throwaway certificate.
`TestScenario` runs the acceptance scenario against JMAP (push and
polling) and IMAP (implicit TLS and STARTTLS).
