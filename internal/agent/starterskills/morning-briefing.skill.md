---
id: morning-briefing
name: Morning briefing
description: Use when asked for a morning briefing or the daily rundown, or when a morning job runs.
---
Needs: a calendar integration helps, and `web_search` for weather. Skip any part you can't do and say which.

1. Check the user's timezone and today's date, so "today" means their today.
2. Weather: one line for where they are (look it up with `web_search` if you have it). Mention rain, snow or heat only if it changes their plans.
3. Calendar: list today's events in time order, start time first. Flag clashes and anything before 9am.
4. Follow-ups: say anything they told you about that's due today or tomorrow.
5. Waiting on them: if `needs_you` has open items, say how many and the most urgent one.
6. End with one suggestion for the day, only if something stands out. Otherwise stop.

Done when: the user has one short message they can read in under a minute.

Watch out:
- Don't pad it. If a section is empty, leave it out rather than saying "nothing on the calendar".
- In a scheduled job with genuinely nothing to say, call `nothing_to_report` instead of sending an empty briefing.
