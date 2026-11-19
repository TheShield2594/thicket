# Running Thicket

What to reach for when something seems off, or to see where the money
goes. All commands run inside the container:
`docker compose exec thicket thicket <command>`.

## `thicket doctor`

Checks everything in one go and exits non-zero if something required is
broken:

```
[ok  ] config           required settings present
[ok  ] database         schema v10, integrity ok
[ok  ] secrets          all stored secrets decrypt
[ok  ] model server     OpenRouter reachable, key accepted
[ok  ] discord token    valid, bot thicket
[ok  ] mcp.homeassistant responding, 14 tools
[warn] mcp.files        not responding: connection refused
[ok  ] server           running, version 0.1.0
[ok  ] live discord     connected
[ok  ] live scheduler   last tick 2026-11-16T19:44:10Z
```

- **model server**: `GET /models` on the configured server, and on
  OpenRouter, the stored key checked against `/key`.
- **discord token / telegram token**: one REST call each (`/users/@me`,
  `getMe`). Tokens never appear in the output.
- **mcp.\***: each enabled integration is connected and asked for its
  tools. A down integration is a warning, not a failure.
- **server / live \***: the running server's `/health`, for what only it
  knows: whether the chat apps are connected right now and the scheduler
  is ticking. If Thicket isn't running, this is a warning.

`/health` itself now lists each integration as `mcp.<name>` with
`"optional": true`: reported, but it never makes `/health` fail (so a
broken integration can't get the container restarted).

## `thicket costs`

```
thicket costs            # last 7 days (same as --week)
thicket costs --month    # this calendar month, UTC
thicket costs --days 30
thicket costs --json
```

Spend per sprig split into **heartbeat** (work Thicket starts itself:
scheduled and webhook jobs, check-ins, follow-ups, memory review) and
**task** (chats and work you asked for: replies, delegated tasks, previews,
voice checks), then per model and per day, with tokens. The origin is fixed
when the work starts, so a consistency check inside a scheduled job counts
as heartbeat. Costs are what OpenRouter reported for each call; local model
servers count as $0.

Every chat reply re-sends part of the conversation: up to `history_tokens`
of earlier messages (plus the summary and pins) and any PDF text kept for
follow-ups (`document_chars`). The report says what share of chat spend
that was, for example:

```
Of $4.10 chat spend (212 model calls measured), about 41% (≈$1.68) went on
re-sending earlier conversation (history_tokens) and 9% (≈$0.37) on PDF
text kept for follow-ups (document_chars).
```

The dashboard's Costs section shows the same for the month, and so does
asking Juni "what have we spent?". These are estimates: Thicket measures
each prompt's make-up in characters, and the provider reports one cost
per call, which is split between prompt and reply assuming an output
token costs 4 times an input token. Calls from before this was measured
aren't counted. If history is most of it, ask Juni to lower
`history_tokens` (say, to 6000); if documents are, lower `document_chars`
or set it to 0 to keep none. The 80% budget warning names whichever
setting is worth lowering.

### Spend caps

`THICKET_DAILY_BUDGET_USD` (default 2) and `THICKET_MONTHLY_BUDGET_USD`
(default 30), also changeable in chat. At 80% of a cap the admins get a
warning; at 100%:

- heartbeats are paused: due jobs are skipped (shown as "skipped: the
  monthly spend cap is reached" in the job's history, not as failures, and
  not counted as missed afterwards), and check-ins and follow-ups wait;
- every other model call is refused too, so the cap is a hard limit.

Raise the cap in chat or wait for the period to reset; heartbeats resume
on their next scheduled time.

## The "needs you" inbox

Nothing that fails is dropped silently. These land in the inbox:

| Kind | When |
|---|---|
| `failed` | a scheduled job or a delegated task failed |
| `interrupted` | Thicket stopped while a job or task was running |
| `undelivered` | it worked, but the message couldn't be sent (the result is kept) |
| `draft` | a sprig at `draft` level wrote up an action instead of doing it |

A job failing every hour is one item with a count, not a pile. The chat
report of a failure is still sent at most every 6 hours; the inbox gets
every one. Pending approvals are listed alongside.

- In chat: ask Juni "what needs me?" (`needs_you`), and to resolve an item.
- From the shell: `thicket inbox`, `thicket inbox resolve 12`.

### Restarts

At startup, job runs and delegated tasks that were in progress when
Thicket stopped are failed cleanly: each goes to the inbox as
`interrupted`, and the sprig tells you once the chat apps are connected.
They aren't re-run automatically, because a half-finished run may already
have acted (sent an email, switched something on), and running it again
could do that twice. Scheduled jobs simply run again at their next time.

### Retries

Model calls are retried with exponential backoff (1s, 2s), then fall back
to `THICKET_FALLBACK_MODEL`. Integration calls that only read are retried
twice (500ms, 1s) after reconnecting if the connection drops. Calls that
change something are never retried: if the connection drops mid-call,
nobody can tell whether it happened, so the sprig says so instead.

## `thicket voice-check SPRIG`

Replays a sprig's reference exchanges against the current model and
prompt, and has the cheap model judge each new reply against the
reference on a short rubric: tone, length, catchphrases, stays in role.
The never-say list is also checked in code. Run it after changing models
or editing a character.

```
Voice check for Juni: 5 test(s), model anthropic/claude-sonnet-5.5, judge anthropic/claude-haiku-5.5

ok     01-greeting
DRIFT  02-something-broke: tone. Upbeat where the reference owns the problem plainly.
       you: my morning summary never showed up today. what happened?
       ref: It didn't run, and that's on me to check, not you. ...
       now: Oh no! Let's figure this out together! ...
ok     03-new-sprig
...
4 of 5 in voice, 1 drifted, 0 errors
```

It exits non-zero on drift, so it can run in a script. The reply is the
model's raw answer, before the consistency rewrite chat replies get, so
drift isn't hidden. Each run costs ten small model calls.

Tests live next to the character file, one YAML file each:

```
characters/
  juniper.md
  juniper/
    voice_tests/
      01-greeting.yaml
```

```yaml
prompt: hey juni, how's it going?
reference: |-
  Hey you! All quiet in the garden, which means I've had time to tidy my lists. How's your day shaping up?
notes: A plain hello. Short, warm, a question back. At most one garden word.
```

Juni ships with five. For your own sprigs, write prompts only and run
`thicket voice-check pip --record` once when you're happy with how Pip
sounds: replies are saved as the references for tests that have none.
Voice tests are included in `thicket backup` and restored with it.

## People and sign-in

```
thicket user list                          # id, name, role, how each signs in
thicket user add kim --name Kim            # a web login; password from stdin or typed twice
thicket user add sam --user 3              # give user 3 (from Discord, say) a login
thicket user passwd kim                    # new password; signs out kim's sessions
thicket user email 3 sam@example.com       # match single sign-on to user 3
thicket user invite --role member --ttl 72h
```

`thicket web link` still prints a one-time sign-in link. Sessions are
listed, and can be signed out, on the dashboard and in the web app.

## Secrets

Keys and tokens live in the database, encrypted with AES-256-GCM under a
master key: `THICKET_SECRET_KEY`, else the file `THICKET_SECRET_KEY_FILE`
names (for Docker secrets), else `/data/secret.key`, created on first run.
Keep the master key off the volume if you can: then a copy of the
volume alone doesn't reveal the secrets.

`DISCORD_TOKEN`, `TELEGRAM_TOKEN`, `OPENROUTER_API_KEY`,
`THICKET_MODEL_API_KEY` and `THICKET_OIDC_ISSUER`, `_CLIENT_ID` and
`_CLIENT_SECRET` given in the environment are stored on start, and
the log says each can now be removed from your compose file. Thicket
reads them from the store from then on. If one is still in the
environment at startup, its value wins and replaces the stored one (so
an orchestrator that rotates it keeps working).

```
thicket secrets list                 # names and where each comes from; never values
thicket secrets set discord_token    # reads the value from stdin
thicket secrets delete github_token
thicket secrets rotate-key           # new master key, every secret re-encrypted
```

In Docker, `set` reads from a pipe or prompts:
`docker compose exec -T thicket thicket secrets set telegram_token < token.txt`.
New chat tokens take effect at the next restart.

`rotate-key` writes the new key first, re-encrypts every secret in one
transaction, then swaps the key files (`secret.key.old` keeps the old one
until you delete it). Restart Thicket straight after: until then the
running server holds the old key and can't read secrets. When the key
comes from `THICKET_SECRET_KEY` or `THICKET_SECRET_KEY_FILE`, Thicket can't
change it, so pass `--out FILE`, put the new key where the old one was,
and restart.

Values never reach logs, the trail, the inbox, conversations, prompts, API
responses, the web app or the dashboard: every one of those passes
through the redactor, which knows each stored value and credential-shaped
strings, and a test plants each known secret and checks every sink.

## `thicket init`

First-run setup at a terminal, as an alternative to environment variables
and to Juni asking in chat: `docker compose run --rm thicket init` (or
`thicket init` outside Docker). It asks, in order, for the model (an
OpenRouter key, or your own server's address and which of its models to
use), the Discord and Telegram bot tokens, the household's timezone, the
daily and monthly spend caps, and the owner's chat account
(`discord:USER_ID` or `telegram:USER_ID`).

Each answer is checked before it is stored: the key against OpenRouter,
a local server by listing its models, the tokens against Discord and
Telegram, the timezone by name. A wrong answer is explained and asked
again; Enter skips. Keys and tokens go into the encrypted store and are
never shown again; the rest are ordinary settings
(`thicket settings list`). Nothing is written to a plain config file.

- `--from-env` doesn't ask: it checks and stores what the environment has
  (`OPENROUTER_API_KEY`, `THICKET_MODEL_URL`, `DISCORD_TOKEN`,
  `TELEGRAM_TOKEN`, `TZ`, the budget variables, `THICKET_OWNER_*`) and
  exits non-zero, naming the variable, if one fails its check.
- `--no-verify` stores without checking, for a machine that's offline.
- Run it again any time. It shows what's set (secrets only as "stored")
  and changes what you type.

It ends with how to start Thicket and a one-time sign-in link for the web
app. Juni's onboarding doesn't ask again for what's already set: with a
key stored she greets you straight away, and with a timezone set she asks
only your name. Naming the owner here also means nobody else can claim a
fresh install by messaging the bot first.

## `thicket update`

Updates are manual, and run from the version you're moving **to**, with
Thicket stopped:

```bash
docker compose pull                      # after changing THICKET_VERSION, if you pin one
docker compose stop thicket
docker compose run --rm thicket update
docker compose up -d
```

Outside Docker, replace the binary and run `thicket update` before
starting it. The command:

1. Shows the changelog between the version that last ran this data and
   this one, and asks to continue (`--yes` skips the question).
2. Sets the database files and sprig folders aside untouched, and writes
   a full backup to `/data/backups/pre-update-DATE.tar.gz` in the same
   format as `thicket backup`.
3. Applies the migrations.
4. Runs the doctor's checks. The ones about your data (configuration,
   database integrity, stored secrets) decide the outcome; a model server
   or chat app that can't be reached is shown as a warning and doesn't
   undo anything.

If a migration or a data check fails, the database files and sprig
folders are put back byte for byte, the reason is printed, and the
command exits non-zero: keep running the previous version. The backup is
kept either way.

It refuses to run while Thicket is running on the data, to go back
across a schema change (start the newer version again, or restore the
pre-update backup with the older one), and to go to an older version on
the same schema without `--force`. `--to VERSION` is a guard: it refuses
unless this binary is that version.

Each successful update is recorded in the trail. Every attempt, including
ones that were rolled back, is a line in `/data/update.log`:
`thicket update history` prints them.

Starting a new version without running `thicket update` still works as
before (startup applies pending migrations after taking a snapshot), but
nothing is checked and nothing rolls back by itself.

## Releasing

Versions are tags. To release `v0.2.0`: rename the top section of
`CHANGELOG.md` to `## v0.2.0: ...` (and start a new `## Unreleased` above
it for what comes next), commit, then `git tag v0.2.0 && git push origin
v0.2.0`. The **release** workflow runs the tests, builds images for
amd64 and arm64 tagged `0.2.0`, `0.2` and `latest`, checks that each
reports the version, and creates the GitHub release with that changelog
section as its notes. A tag with no section in the changelog fails before
anything is published (`thicket changelog v0.2.0` is the same check).
Pre-release tags such as `v0.2.0-rc.1` publish only their own tag. Pushes
to `main` publish the `edge` tag.

## `thicket migrate`

Every schema change is a numbered SQL file in
`internal/store/migrations`, applied in order, each in a transaction, with
a database snapshot taken first. Startup applies pending migrations; this
command does the same and lists what's applied. A database newer than the
binary is refused. A test fails the build if any Go code in the store
creates or alters tables outside a migration.
