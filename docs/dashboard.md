# Dashboard

One page at `/dashboard` with what Thicket knows and does for you. It's
read-mostly: the only things you can do here are answer what needs you,
pause, and change what sprigs remember about you.

- **Sprigs**: the roster, each with its drawn avatar showing what it's
  doing right now (idle, thinking, working, sleeping in its quiet hours,
  or stuck when it has failed work in your inbox from the last day), or
  how its last reply went for a few minutes after (pleased, or unsure
  after a rewrite, a refusal or while it waits on your OK). The page
  checks every 5 seconds. Each card also shows how many of the
  sprig's daily unprompted messages you've had today, how many are held
  for later (quiet hours or the daily limit), and its quiet hours.
- **Needs you**: requests waiting for your OK (approve or deny), drafts a
  sprig wrote up instead of doing (do it, or dismiss), and failed,
  interrupted or undelivered work (clear it once you've dealt with it).
  An email a sprig drafted shows who it's to, the subject and why it
  wasn't sent, with **Send** and **Discard**. When a one-time code
  arrives in a sprig's mailbox, an item here gives the sender and
  subject; the code itself is in the sprig's mailbox (**Mail**), where
  you read it.
  The sprig reports back in the chat the request came from.
- **Pause**: the owner has **Pause everything**, and each sprig you own
  has a **Pause** button (the person a private sprig belongs to, or the
  owner or an admin for a shared one). While paused, the page says so
  and offers **Resume**. See [Pausing](operations.md#pausing-the-kill-switch).
- **Trail**: what sprigs did, newest first: tools used, approvals,
  drafts, handoffs between sprigs, jobs.
- **What they remember about you**: relationship memory (how things are
  going with each sprig: how you like it to talk to you, what landed,
  what annoyed you, in-jokes) apart from facts, each with where it came
  from and when. Edit, lock or unlock, undo your last change, or delete
  any of them, and add your own. Edits are kept as you wrote them
  (locked); deleting forgets for good. Parents also see and change what
  each kid sprig they set up remembers about its kids.
- Your jobs and their recent runs, your
  recent background tasks (work one sprig gave another, with how it
  ended: done, nothing found, failed, stopped, or interrupted by a
  restart), and the skill library.

Owners and admins also see **costs** (today and this month against the
caps; this month per sprig split into heartbeat and task, and per model;
the last 14 days) and **health**, and the trail of everyone's sprigs.
Everyone else sees only their own memories, jobs, inbox and trail.

Someone else's solo sprig is private, admins included: the roster shows
it as "Kim's private sprig" with only its state and whether it has
logged errors in the last day, and Costs show what it spends under that
label. Its name, role, tools, activity and changes never appear.

## Mail

With the [mail plugin](mail.md) running, **Mail** (top of the page)
opens each sprig's mailbox you may see: inbox and drafts, newest first.

- **Who sees it**: a private sprig's mailbox only for the person it
  belongs to (admins don't see it, or that it has one); a shared
  sprig's for every adult, not kids. Kid sprigs have no mail.
- **Reading**: text only. The plugin turns HTML into text on the server
  and the page escapes every byte of it: nothing runs, no remote images
  are loaded, and links are written out with where they really go
  (`text <https://real.target>`). Open one only if you trust it.
- **Attachments** are listed with their name, type and size, and only
  download when you click, always as a file (never opened in the page).
- **Drafts**: **Send** or **Discard**, the same as in "needs you" (which
  is cleared too).
- **Allowlist**: who the sprig may email without you. Its owner adds and
  removes addresses or `@domain`s here; every change is in the trail and
  can be undone. Others see the list.

## Browser

With the [browser plugin](browser.md) running, **Browser** (top of the
page) lists the sprigs you may see that have the browser, each with the
sites its browser may load. Its owner allows and removes sites there;
every change is in the trail and can be undone. Others see the list. A
private sprig's list is only for its person; kids see none.

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
`/avatars/<id>.svg?state=thinking`. The states are idle, thinking,
working, sleeping, stuck, pleased and unsure (the last two follow how a
sprig's last reply went: see [Avatars](characters.md#avatars)).
