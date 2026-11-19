-- What sprigs said in the messages they started themselves (job and
-- trigger output, check-ins, follow-ups), so the worth-saying gate can
-- tell a rerun from news. Only the newest few per sprig and person are
-- kept; sent_at also marks when the sprig last wrote to them.
CREATE TABLE proactive_said (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character TEXT    NOT NULL,
    text      TEXT    NOT NULL,
    sent_at   INTEGER NOT NULL
);
CREATE INDEX proactive_said_lookup ON proactive_said (user_id, character, id);
