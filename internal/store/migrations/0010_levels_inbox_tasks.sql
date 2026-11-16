-- Permission levels per sprig and tool, the "needs you" inbox, delegated
-- tasks that survive a restart, and heartbeat/task tagging of model usage.

-- level: read (only read-only tools run), draft (changes are written up
-- for the user, never run), ask (changes need approval), act (no approval).
-- character '*' applies to every sprig; a sprig's own row wins.
CREATE TABLE tool_levels (
    character  TEXT    NOT NULL,
    tool       TEXT    NOT NULL,
    level      TEXT    NOT NULL CHECK (level IN ('read', 'draft', 'ask', 'act')),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (character, tool)
);

-- The old global approval rules become levels for every sprig.
INSERT INTO tool_levels (character, tool, level, updated_at)
    SELECT '*', tool, CASE approval WHEN 'always' THEN 'ask' ELSE 'act' END, updated_at FROM tool_policies;
DROP TABLE tool_policies;

-- Things that need a person: failed or interrupted work, undelivered
-- results, drafts. Pending approvals live in approvals and are shown
-- alongside.
CREATE TABLE inbox (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character   TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL CHECK (kind IN ('failed', 'interrupted', 'undelivered', 'draft')),
    ref         TEXT    NOT NULL DEFAULT '', -- job:ID, task:ID, tool:NAME
    title       TEXT    NOT NULL,
    detail      TEXT    NOT NULL DEFAULT '',
    count       INTEGER NOT NULL DEFAULT 1,   -- repeats of the same open item
    status      TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    resolved_at INTEGER
);
CREATE INDEX inbox_open ON inbox (status, user_id);

-- Background tasks one sprig gives another (delegate_task).
CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    from_char   TEXT    NOT NULL,
    character   TEXT    NOT NULL,
    prompt      TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('running', 'ok', 'empty', 'error', 'stopped', 'interrupted')),
    error       TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX tasks_status ON tasks (status);

-- heartbeat: work Thicket starts by itself (jobs, check-ins, follow-ups,
-- memory review). task: work a person asked for (chat, delegated tasks).
ALTER TABLE usage ADD COLUMN origin TEXT NOT NULL DEFAULT '';
UPDATE usage SET origin = CASE WHEN purpose IN ('job', 'proactive', 'memory') THEN 'heartbeat' ELSE 'task' END;
