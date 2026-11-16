-- Event triggers for jobs, and the exact action behind a drafted inbox item.

-- A trigger makes a job fire on an event instead of the clock. The job
-- itself is kind 'webhook' (started by code, never by the scheduler tick).
-- source 'mcp': poll a read-only integration tool every N minutes and fire
--   with the items that are new since the last poll (and match, if set).
-- source 'webhook': the job's URL fires it only when the body matches.
CREATE TABLE triggers (
    job_id       INTEGER PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    source       TEXT    NOT NULL CHECK (source IN ('mcp', 'webhook')),
    server       TEXT    NOT NULL DEFAULT '',
    tool         TEXT    NOT NULL DEFAULT '',
    args         TEXT    NOT NULL DEFAULT '{}',
    every_min    INTEGER NOT NULL DEFAULT 15,
    match        TEXT    NOT NULL DEFAULT '',
    seen         TEXT    NOT NULL DEFAULT '[]', -- keys of items already seen
    primed       INTEGER NOT NULL DEFAULT 0,    -- first poll only records what's there
    last_poll_at INTEGER,
    last_error   TEXT    NOT NULL DEFAULT ''
);

-- For drafts: {"tool": ..., "args": ..., "chat_ref": ...}, so the user can
-- have it done from the dashboard.
ALTER TABLE inbox ADD COLUMN action TEXT NOT NULL DEFAULT '';
