-- Sprig email addresses (Phase 4, thicket-mail plugin). One address per
-- sprig; its password lives in the encrypted secret store under
-- secret_name, never here. created_by is who set it up (the trail and
-- "needs you" items for a shared sprig's mail go to them).
CREATE TABLE mail_accounts (
    sprig          TEXT PRIMARY KEY,
    address        TEXT NOT NULL,
    backend        TEXT NOT NULL,           -- jmap | imap
    settings       TEXT NOT NULL DEFAULT '{}', -- JSON: server addresses and username, no secrets
    secret_name    TEXT NOT NULL,
    retention_days INTEGER NOT NULL DEFAULT 90,
    send_limit     INTEGER NOT NULL DEFAULT 20,
    created_by     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

-- Who a sprig may email without asking: exact addresses or @domain. Only
-- the sprig's owner changes it; every change is in the trail.
CREATE TABLE mail_allow (
    sprig    TEXT NOT NULL,
    entry    TEXT NOT NULL,
    added_by INTEGER NOT NULL DEFAULT 0,
    added_at INTEGER NOT NULL,
    PRIMARY KEY (sprig, entry)
);

-- "needs you" gains a kind: a one-time code that arrived in a sprig's
-- mailbox, for a person to read (the sprig sees it hidden). SQLite can't
-- change a CHECK, so the table is rebuilt (nothing references it).
CREATE TABLE inbox_new (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character   TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL CHECK (kind IN ('failed', 'interrupted', 'undelivered', 'draft', 'code')),
    ref         TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL,
    detail      TEXT    NOT NULL DEFAULT '',
    count       INTEGER NOT NULL DEFAULT 1,
    status      TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    resolved_at INTEGER,
    action      TEXT    NOT NULL DEFAULT ''
);
INSERT INTO inbox_new (id, user_id, character, kind, ref, title, detail, count, status, created_at, updated_at, resolved_at, action)
    SELECT id, user_id, character, kind, ref, title, detail, count, status, created_at, updated_at, resolved_at, action FROM inbox;
DROP TABLE inbox;
ALTER TABLE inbox_new RENAME TO inbox;
CREATE INDEX inbox_open ON inbox (status, user_id);
