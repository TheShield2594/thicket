# The sandbox: sprigs' own computer

Thicket comes with a second container, `thicket-sandbox`: a small Debian
computer sprigs can use to run commands and keep files. Ask a sprig to
clone a repo and run its tests, tidy a CSV with Python, try a curl
command, or draft a script, and it does the work there and tells you
what happened.

## What it is

- Debian with bash, git, curl, wget, jq, python3 (with pip and venv),
  sqlite3, zip and a few editors. Sprigs can `pip install` more; it goes
  into the sandbox's home folder, which is kept.
- `/workspace` keeps files between commands and restarts (a Docker
  volume). Nothing of yours is on it unless a sprig puts it there.
- It can't see Thicket's database, keys or character files, and it isn't
  published on any port. It runs as an ordinary user, with no added
  privileges, 2 CPUs, 2 GB of memory and at most 512 processes (change
  these in `docker-compose.yml`).
- It **does** have internet access, so sprigs can download things. To
  cut it off, put the `sandbox` service on an `internal: true` network
  that only Thicket shares, or remove the service.

## Using it

The sandbox shows up as the `sandbox` integration. No sprig can use it
until you say so:

> Juni, give Pip the computer.

Juni grants `mcp:sandbox/*`. The tools are:

| Tool | Default |
|---|---|
| `run_command` | asks you first, showing the exact command |
| `write_file` | asks you first |
| `read_file`, `list_files` | run freely |

Commands time out after 60 seconds (a sprig can ask for up to 10
minutes), output is cut to the last 16 KB of each stream, and anything a
command leaves running in the background is stopped when it finishes.
What comes back is treated as untrusted data, like any integration.

Asking for every command gets old quickly. To let a sprig work on its
own: "Juni, let Pip run commands without asking"
(`set_tool_level(mcp_sandbox__run_command, act, sprig: pip)`). Only do
that for a sprig you trust with an internet-connected machine: a web page
or file it reads could try to talk it into something. See
[permission levels](mcp.md#approvals-and-permission-levels).

## Setup and turning it off

`docker compose up -d` starts both containers. On start Thicket writes a
random token into a small shared volume (`sandbox-auth`), keeps a copy in
its encrypted store, and connects. Nothing to configure.

- **Off:** set `THICKET_SANDBOX_URL` to an empty value and remove the
  `sandbox` service.
- **Start over:** `docker compose down`, then remove the
  `sandbox-workspace` and `sandbox-home` volumes.
- **Your own image:** build on `ghcr.io/theshield2594/thicket-sandbox`
  and add what you need (Node, a compiler) with `apt-get`, or point
  `THICKET_SANDBOX_URL` at your own build of `cmd/thicket-sandbox`.
