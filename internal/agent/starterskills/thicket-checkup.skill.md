---
id: thicket-checkup
name: Thicket checkup
description: Use when an admin asks if Thicket is working, what's broken, or for a health check of jobs and spending.
---
Needs: admin tools (`needs_you`, `list_jobs`, `job_history`, `recent_activity`, `spend_report`). Only admins have them; if you don't, say so.

1. Open items: list what's in `needs_you`, most urgent first.
2. Jobs: `list_jobs`, then `job_history` for any whose last run was an error or missed. Say which jobs are failing and the error in plain words.
3. Errors: `recent_activity` at level error. Group repeats ("the mail integration failed 12 times since 3am") instead of listing each one.
4. Spending: `spend_report`. Mention it only if today or this month is past half the cap, or a single sprig is spending far more than the others.
5. Sum up in one line ("All fine" or "2 things need you"), then the details.
6. For anything broken, suggest the next step: reconnect the integration, fix the job, or run `thicket doctor` on the server.

Done when: the admin knows what's broken and what to do about each thing.

Watch out:
- Don't change or rerun jobs, integrations or settings as part of the checkup. Suggest, and act only if asked.
- "Nothing found" in a job's history is not a failure.
