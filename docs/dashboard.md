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

Ask Juni: "send me a dashboard link". Thicket DMs you a link that works
once, within 10 minutes; opening it signs that browser in for a week.
Juni never sees the link. On the server, `thicket dashboard-link` prints
one for the owner (or `--user discord:ID`). "Sign out" ends the session.

## Reaching it

The dashboard is served on port 8080 at `/dashboard`, which the compose
file publishes only to the host itself. Either:

- set `THICKET_PUBLIC_URL` and expose `/dashboard` through your HTTPS
  reverse proxy (links then use that address, and the cookie is marked
  secure); or
- open a tunnel from your computer, `ssh -L 8080:localhost:8080 your-server`,
  and use the link as sent (`http://localhost:8080/...`).

The page uses no JavaScript, can't be embedded in other sites, and is
never cached.
