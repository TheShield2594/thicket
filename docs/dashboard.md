# Dashboard

A read-only web page with what Thicket knows and does for you:

- your sprigs and what each may use;
- what they remember about you, shared and per sprig, with follow-ups;
- your jobs and their recent runs (result, errors, cost);
- the skill library.

Owners and admins also see spending (today, this month against the caps,
by sprig, and the last 14 days), health, and the recent activity log.
Everyone sees only their own memories and jobs; admins don't see other
people's memories.

To change anything, ask a sprig: the page only shows.

## Signing in

The dashboard uses the web app's sign-in. Open the web app (ask Juni for a
web app link, or run `thicket web link` on the server), then go to
`/dashboard`; the page also links back to the app. Signing out of the web
app signs you out here too.

## Reaching it

The dashboard is served on port 8080 at `/dashboard`, which the compose
file publishes only to the host itself. Either:

- set `THICKET_PUBLIC_URL` and expose the web app (including
  `/dashboard`) through your HTTPS reverse proxy; or
- open a tunnel from your computer, `ssh -L 8080:localhost:8080 your-server`,
  and use `http://localhost:8080/dashboard`.

The page uses no JavaScript, can't be embedded in other sites, and is
never cached.
