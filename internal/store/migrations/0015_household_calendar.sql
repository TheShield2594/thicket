-- The household model, the reversible trail, and the calendar.

-- Kids are members with kid = 1 (the role check predates them). A kid can't
-- be an admin; the store reports their role as "kid".
ALTER TABLE users ADD COLUMN kid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE invites ADD COLUMN kid INTEGER NOT NULL DEFAULT 0;

-- Family memory: what family sprigs learn about the household, kept apart
-- from each person's private memories. user_id is who it came from.
ALTER TABLE memories ADD COLUMN household INTEGER NOT NULL DEFAULT 0;
CREATE INDEX memories_household ON memories (household, character);

-- The trail: every permission change and every sprig-created event, with
-- what it takes to undo it. Kept (unlike the activity log, which is pruned).
CREATE TABLE trail (
    id         INTEGER PRIMARY KEY,
    at         INTEGER NOT NULL,
    user_id    INTEGER REFERENCES users (id) ON DELETE SET NULL, -- who made the change (or for whom)
    character  TEXT    NOT NULL DEFAULT '',
    kind       TEXT    NOT NULL, -- level | grant | event | role | safety | undo
    summary    TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '',
    undo       TEXT    NOT NULL DEFAULT '', -- JSON; '' = nothing to undo
    undone_at  INTEGER,
    undone_by  INTEGER REFERENCES users (id) ON DELETE SET NULL
);
CREATE INDEX trail_at ON trail (at);

-- Calendars adults connect, read-only: an ICS feed or a CalDAV calendar.
-- shared = 1 shows it to every adult in the household.
CREATE TABLE calendar_sources (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('ics', 'caldav')),
    url        TEXT    NOT NULL,
    username   TEXT    NOT NULL DEFAULT '',
    secret     TEXT    NOT NULL DEFAULT '', -- name of the password in the encrypted store
    shared     INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Sprig outputs: things sprigs put on the calendar (a dinner from a meal
-- planner, a release date), each linking back to its source.
CREATE TABLE sprig_events (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character    TEXT    NOT NULL,
    kind         TEXT    NOT NULL DEFAULT 'event', -- e.g. dinner, release, reminder, event
    title        TEXT    NOT NULL,
    starts_at    INTEGER NOT NULL,
    ends_at      INTEGER,
    all_day      INTEGER NOT NULL DEFAULT 0,
    details      TEXT    NOT NULL DEFAULT '',
    source_label TEXT    NOT NULL DEFAULT '',
    source_url   TEXT    NOT NULL DEFAULT '',
    shared       INTEGER NOT NULL DEFAULT 0,
    deleted_at   INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX sprig_events_time ON sprig_events (starts_at);
