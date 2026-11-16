-- Thicket schema v1. Timestamps are unix seconds (UTC).

CREATE TABLE users (
    id           INTEGER PRIMARY KEY,
    channel      TEXT    NOT NULL,
    external_id  TEXT    NOT NULL,
    name         TEXT    NOT NULL DEFAULT '',
    role         TEXT    NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    timezone     TEXT    NOT NULL DEFAULT '',
    onboarded_at INTEGER,
    last_seen_at INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (channel, external_id)
);

CREATE TABLE conversations (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chat_ref         TEXT    NOT NULL,
    started_at       INTEGER NOT NULL,
    last_activity_at INTEGER NOT NULL,
    reviewed_at      INTEGER,
    review_attempts  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX conversations_open ON conversations (user_id, chat_ref, last_activity_at);

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'assistant', 'event')),
    character       TEXT    NOT NULL DEFAULT '',
    content         TEXT    NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX messages_conv ON messages (conversation_id, id);

-- character = '' means the shared layer every character can see.
CREATE TABLE memories (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character    TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    source       TEXT    NOT NULL CHECK (source IN ('auto', 'user')),
    locked       INTEGER NOT NULL DEFAULT 0,
    due_at       INTEGER,
    expires_at   INTEGER,
    last_used_at INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX memories_user ON memories (user_id, character);

CREATE TABLE extraction_runs (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER REFERENCES conversations (id) ON DELETE SET NULL,
    user_id         INTEGER NOT NULL,
    model           TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL,
    error           TEXT    NOT NULL DEFAULT '',
    applied         INTEGER NOT NULL DEFAULT 0,
    skipped         INTEGER NOT NULL DEFAULT 0,
    rolled_back_at  INTEGER,
    created_at      INTEGER NOT NULL
);

-- Every automatic change to memory is recorded so a bad run can be rolled back.
CREATE TABLE memory_events (
    id         INTEGER PRIMARY KEY,
    memory_id  INTEGER NOT NULL,
    run_id     INTEGER,
    action     TEXT    NOT NULL,
    before     TEXT,
    after      TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX memory_events_run ON memory_events (run_id);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character   TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    schedule    TEXT    NOT NULL,
    prompt      TEXT    NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at INTEGER,
    created_at  INTEGER NOT NULL
);

CREATE TABLE job_runs (
    id          INTEGER PRIMARY KEY,
    job_id      INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    status      TEXT    NOT NULL CHECK (status IN ('running', 'ok', 'empty', 'error', 'missed')),
    output      TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    cost_usd    REAL    NOT NULL DEFAULT 0
);
CREATE INDEX job_runs_job ON job_runs (job_id, id);

CREATE TABLE activity (
    id        INTEGER PRIMARY KEY,
    at        INTEGER NOT NULL,
    level     TEXT    NOT NULL,
    kind      TEXT    NOT NULL,
    character TEXT    NOT NULL DEFAULT '',
    user_id   INTEGER,
    summary   TEXT    NOT NULL,
    detail    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX activity_at ON activity (at);

CREATE TABLE usage (
    id                INTEGER PRIMARY KEY,
    at                INTEGER NOT NULL,
    user_id           INTEGER,
    character         TEXT    NOT NULL DEFAULT '',
    job_id            INTEGER,
    purpose           TEXT    NOT NULL DEFAULT '',
    model             TEXT    NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL    NOT NULL DEFAULT 0
);
CREATE INDEX usage_at ON usage (at);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE secrets (
    name       TEXT PRIMARY KEY,
    nonce      BLOB    NOT NULL,
    ciphertext BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE mcp_servers (
    name       TEXT PRIMARY KEY,
    transport  TEXT    NOT NULL CHECK (transport IN ('stdio', 'http')),
    target     TEXT    NOT NULL,
    args       TEXT    NOT NULL DEFAULT '[]',
    env        TEXT    NOT NULL DEFAULT '{}',
    headers    TEXT    NOT NULL DEFAULT '{}',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE tool_policies (
    tool       TEXT PRIMARY KEY,
    approval   TEXT    NOT NULL CHECK (approval IN ('always', 'never')),
    updated_at INTEGER NOT NULL
);

CREATE TABLE approvals (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character  TEXT    NOT NULL,
    chat_ref   TEXT    NOT NULL,
    tool       TEXT    NOT NULL,
    args       TEXT    NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    result     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    decided_at INTEGER
);

-- Hashes of character files Thicket wrote itself, so user edits are never clobbered.
CREATE TABLE character_files (
    id         TEXT PRIMARY KEY,
    sha256     TEXT    NOT NULL,
    written_at INTEGER NOT NULL
);

CREATE TABLE pending_secrets (
    user_id    INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    purpose    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);

-- De-duplication of alerts and proactive messages.
CREATE TABLE notices (
    key     TEXT PRIMARY KEY,
    sent_at INTEGER NOT NULL
);
