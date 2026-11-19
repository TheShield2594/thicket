# Thicket as an MCP server

`thicket mcp` lets an AI agent such as Claude Desktop, Claude Code or
Cursor talk to your sprigs over the Model Context Protocol. The agent can
ask a sprig something ("ask Pip what's on tomorrow"), list your sprigs and
jobs, run a job, and stop a reply.

(Thicket can also *use* MCP servers as integrations; that's
[Integrations](mcp.md).)

## What an agent can and can't do

| Tool | What it does |
|---|---|
| `ask_sprig` | Sends a message and returns the reply (up to 3 minutes when the sprig uses tools). Optional `sprig` (id or name) and `conversation` (default `mcp`). |
| `list_sprigs` | Ids, names and roles. |
| `list_jobs` | Scheduled and webhook jobs with their last run. |
| `run_job` | Runs a job now, with optional `payload`; the result comes to your chat app. |
| `stop` | Stops the reply in progress in a conversation. |

The agent acts as the person its API token belongs to and sees the same
sprigs and memories. It **can't answer approval prompts**: when a sprig
asks for an OK before something risky, the agent is told only you can
give it, and you approve or deny on the dashboard (the "needs you" list)
or in your chat app. Settings, secrets, characters and deleting things
aren't offered at all.

The agent's conversations are kept apart from your chat apps, like any
API conversation. Spend counts toward your caps as usual.

## Setting it up

1. Make a token: `docker compose exec thicket thicket api-token create claude-desktop`.
   Use `--user discord:ID` for a token that acts as someone other than the
   owner.
2. Tell your agent to run `thicket mcp` with that token. It runs inside
   the container through `docker exec`, so it reaches Thicket on its own
   port.

Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "thicket": {
      "command": "docker",
      "args": ["exec", "-i", "-e", "THICKET_API_TOKEN", "thicket", "thicket", "mcp"],
      "env": { "THICKET_API_TOKEN": "thk_api_…" }
    }
  }
}
```

Claude Code:

```bash
claude mcp add thicket -e THICKET_API_TOKEN=thk_api_… -- docker exec -i -e THICKET_API_TOKEN thicket thicket mcp
```

Cursor and others take the same command and arguments.

Running the agent on a different machine from Thicket? Use a `thicket`
binary there and point it at the server with `THICKET_API_URL` (see
[Reaching it](api.md#reaching-it): only expose `/api/v1/` over a
connection you trust).

Revoke the token (`thicket api-token revoke ID`) to cut the agent off.
