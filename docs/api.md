# Local API and CLI

Scripts on the machine running Thicket can message a sprig and get its
reply, list sprigs and jobs, and run a job now. Use it for a build that
should report its result, a cron script, a shell alias, or another tool's
integration.

## Tokens

Every call needs a token. Make one from the command line:

```bash
docker compose exec thicket thicket api-token create laptop
```

The token (`thk_api_…`) is printed once; only a hash is stored. It acts as
the owner unless you pass `--user discord:ID` or `--user telegram:ID`, and
can do what that person can do in chat: the same sprigs, memories,
approvals and spend caps.

```bash
thicket api-token list
thicket api-token revoke 3
```

## From the command line

The `ask`, `jobs` and `run-job` commands talk to the running Thicket, so
they need the token in `THICKET_API_TOKEN` (or a file named by
`THICKET_API_TOKEN_FILE`):

```bash
export THICKET_API_TOKEN=thk_api_…
thicket ask "what's on my calendar tomorrow?"
thicket ask --sprig pip --conversation planning "move the dentist to Friday"
make test 2>&1 | tail -50 | thicket ask --sprig juniper -   # message on stdin
thicket jobs
thicket run-job 4 --payload "deploy finished: v2.3.1"
```

Inside the container these work as they are, against
`http://127.0.0.1:8080`. To run them from the host, use the same binary
(or `docker compose exec -e THICKET_API_TOKEN thicket thicket ask …`), and
set `THICKET_API_URL` if Thicket isn't at `http://127.0.0.1:8080`.

## HTTP

All endpoints take `Authorization: Bearer thk_api_…` and speak JSON.

### `POST /api/v1/messages`

```json
{"text": "what did the CI say?", "sprig": "pip", "conversation": "ci"}
```

`sprig` (id or name) and `conversation` are optional. Without `sprig`,
whoever you spoke to last in that conversation answers, as in chat.
Conversations (up to 40 lowercase letters, digits, `-` or `_`; default
`default`) are separate threads, per token, kept apart from your chat
apps. Memories are shared, as everywhere.

The response comes when the sprigs are done, which can take a while with
tools (up to 3 minutes):

```json
{"conversation": "ci", "replies": [{"sprig_id": "pip", "sprig": "Pip", "text": "…"}]}
```

`replies` can hold several messages: a handoff adds the next sprig's, and
a risky action adds an approval prompt. Answer that with another message
in the same conversation, `{"text": "approve", "conversation": "ci"}`.

A message is handled exactly like a DM: anything that looks like a key or
password is refused, not stored. `429` means more than 20 messages in a
minute.

### `GET /api/v1/sprigs`

`{"sprigs": [{"id": "juniper", "name": "Juni", "role": "…", "host": true}]}`

### `GET /api/v1/jobs`

The token's jobs: `id`, `name`, `kind` (`cron` or `webhook`), `schedule`,
`sprig`, `enabled` and the status of the `last_run`.

### `POST /api/v1/jobs/{id}/run`

Runs a job now, scheduled or webhook, optionally with input:
`{"payload": "…"}` (up to 64 KB; it reaches the sprig marked as outside
data). Returns `202`; the sprig messages you on your chat app with the
result, and the run is in the job history. As with webhooks: `409` if
paused, `429` within a minute of the last run or after 50 runs in a day.

## Reaching it

The API is served on port 8080 with `/health`. The compose file publishes
that port on `127.0.0.1` only, so only the host and the container can call
it. If you put a reverse proxy in front of Thicket for webhooks, don't
expose `/api/` unless you mean to: the token is then all that protects it.
