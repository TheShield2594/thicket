-- Cost estimates made before costly actions (long PDFs, background tasks,
-- long conversations on a big model), and what the action really cost, so
-- the estimates can be checked.
CREATE TABLE estimates (
    id           INTEGER PRIMARY KEY,
    at           INTEGER NOT NULL,
    user_id      INTEGER REFERENCES users (id) ON DELETE SET NULL,
    character    TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL, -- turn | pdf | task | search
    what         TEXT    NOT NULL DEFAULT '',
    estimate_usd REAL    NOT NULL,
    -- asked: the person was asked first; refused: it would have crossed a cap
    outcome      TEXT    NOT NULL DEFAULT 'ran',
    actual_usd   REAL,
    finished_at  INTEGER
);
CREATE INDEX estimates_at ON estimates (at);
