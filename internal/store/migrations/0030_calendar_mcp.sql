-- A calendar read through an integration (kind mcp): config holds the
-- integration, its read-only "list events" tool and how its result maps
-- to events, as JSON. SQLite can't change a CHECK, so the table is
-- rebuilt (nothing references it).
CREATE TABLE calendar_sources_new (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('ics', 'caldav', 'mcp')),
    url        TEXT    NOT NULL,
    username   TEXT    NOT NULL DEFAULT '',
    secret     TEXT    NOT NULL DEFAULT '', -- name of the password in the encrypted store
    shared     INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    config     TEXT    NOT NULL DEFAULT ''
);
INSERT INTO calendar_sources_new (id, user_id, name, kind, url, username, secret, shared, enabled, last_error, created_at)
    SELECT id, user_id, name, kind, url, username, secret, shared, enabled, last_error, created_at FROM calendar_sources;
DROP TABLE calendar_sources;
ALTER TABLE calendar_sources_new RENAME TO calendar_sources;
