-- The kill switch: everything (scope '*') or one sprig (its id) paused.
-- A row exists while paused; resuming deletes it. Kept in the database so
-- a pause survives restarts, and so `thicket pause` works by writing here
-- even while the server is busy.
CREATE TABLE pauses (
    scope     TEXT    PRIMARY KEY,
    paused_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    reason    TEXT    NOT NULL DEFAULT '',
    paused_at INTEGER NOT NULL
);
