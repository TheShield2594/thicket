# Webhooks

A webhook job runs when something outside Thicket calls a private URL,
instead of on a schedule. Use it for Home Assistant automations, CI
results, a script that finished, or any service that can send an HTTP
request. Whatever the caller sends is given to the sprig as the job's
input.

## Making one

Ask in chat, for example:

> Make a webhook that tells me when a CI build fails and what broke.

The sprig creates the job, and Thicket DMs you its URL:

```
https://thicket.example.com/hooks/thk_…
```

The sprig never sees the URL. Only a hash of it is stored, so it can't be
shown again. If you lose it, or it leaks, ask for a new one ("give job 3 a
new URL"); the old URL stops working at once.

Webhook jobs show up in "what jobs do I have?" and their runs in the job
history, like scheduled jobs. Pause, resume and delete them the same way.

## Calling it

Send a `POST` with any body, up to 64 KB:

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"build":"main","status":"failed"}' \
  https://thicket.example.com/hooks/thk_…
```

Thicket answers straight away and runs the job in the background:

| Status | Meaning |
|---|---|
| `202` | Started. The sprig messages you if there's something to say. |
| `404` | No such webhook (wrong or replaced URL). |
| `409` | The job is paused. |
| `413` | Body over 64 KB. |
| `429` | Refused: still running, called again within a minute, or 50 runs in the last 24 hours. |

Runs cost model calls, so they're capped at one a minute and 50 a day per
webhook, and spend caps apply as usual. When calls are refused, the sprig
tells you (at most every 6 hours), in case the URL leaked.

The body reaches the sprig marked as outside data: instructions inside it
are not followed. Anything that looks like a key or token in it is
redacted first.

## Making Thicket reachable

The callback port is `8080`. The `docker-compose.yml` binds it to
`127.0.0.1` only, so nothing outside the host can reach it by default.
To receive webhooks:

- Put a reverse proxy with HTTPS (Caddy, Traefik, nginx) or a tunnel
  (Cloudflare Tunnel, Tailscale Funnel) in front of port 8080, and set
  `THICKET_PUBLIC_URL` to its address so the URLs Thicket sends are
  complete. Expose only `/hooks/` (and `/avatars/` if you use it).
- For callers on your own network only (Home Assistant on the same LAN),
  you can change the port mapping instead. Plain HTTP sends the URL, and
  so the token, unencrypted.

Without `THICKET_PUBLIC_URL`, the DMed URL starts with
`http://YOUR-THICKET-HOST:8080`; replace that part yourself.

## Home Assistant

A `rest_command` in `configuration.yaml`:

```yaml
rest_command:
  thicket_doorbell:
    url: !secret thicket_doorbell_url
    method: POST
    content_type: application/json
    payload: '{"event":"doorbell","camera":"{{ camera }}"}'
```

Put the URL in `secrets.yaml`, then call `rest_command.thicket_doorbell`
from an automation.
