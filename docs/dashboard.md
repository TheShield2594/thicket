# Dashboard

One page at `/dashboard` with what Thicket knows and does for you. It's
read-mostly: the only things you can do here are answer what needs you.

- **Sprigs**: the roster, each with its drawn avatar showing what it's
  doing right now (idle, thinking, working, sleeping in its quiet hours,
  or stuck when it has failed work in your inbox from the last day). The
  page checks every 5 seconds.
- **Needs you**: requests waiting for your OK (approve or deny), drafts a
  sprig wrote up instead of doing (do it, or dismiss), and failed,
  interrupted or undelivered work (clear it once you've dealt with it).
  The sprig reports back in the chat the request came from.
- **Trail**: what sprigs did, newest first: tools used, approvals,
  drafts, handoffs between sprigs, jobs.
- What they remember about you, your jobs and their recent runs, and the
  skill library.

Owners and admins also see **costs** (today and this month against the
caps; this month per sprig split into heartbeat and task, and per model;
the last 14 days) and **health**, and the trail of everyone's sprigs.
Everyone else sees only their own memories, jobs, inbox and trail.

## Signing in

The dashboard uses the web app's sign-in: a username and password, single
sign-on, or a one-time link (see [Signing in](../README.md#signing-in)).
Sign in to the web app, then go to `/dashboard`; the page also links back
to the app. Signing out of the web app signs you out here too.

The **Sign-in** section lists where you're signed in (browser, how, last
active) with a **Sign out** button for each. Admins also see everyone's
sessions and can sign any of them out, and can make an invite link
(member or admin, valid 1 to 7 days); the link is shown once, on the page,
never in a URL.

## Reaching it

The dashboard is served on port 8080 at `/dashboard`, which the compose
file publishes only to the host itself. Either:

- set `THICKET_PUBLIC_URL` and expose the web app (including
  `/dashboard`) through your HTTPS reverse proxy; or
- open a tunnel from your computer, `ssh -L 8080:localhost:8080 your-server`,
  and use `http://localhost:8080/dashboard`.

## Safety

- Its only script is a few lines that fetch `/dashboard/state.json`;
  the Content Security Policy allows nothing else (by nonce).
- Every button posts a form with a token tied to your session, so another
  site can't press them for you. Only the person a request was for can
  answer it.
- It can't be embedded in other sites and is never cached.

Avatars are also served on their own: `/avatars/<id>.svg` (live state) or
`/avatars/<id>.svg?state=thinking`.
