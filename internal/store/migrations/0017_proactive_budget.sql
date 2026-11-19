-- Quiet hours and a daily budget for the messages sprigs start themselves
-- (job and trigger output, check-ins, follow-ups).

-- What was delivered, per sprig and person, so a day's messages can be
-- counted in the person's timezone. Pruned after a few days.
CREATE TABLE proactive_sent (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character TEXT    NOT NULL,
    sent_at   INTEGER NOT NULL
);
CREATE INDEX proactive_sent_lookup ON proactive_sent (user_id, character, sent_at);

-- Finished messages waiting for quiet hours to end (reason 'quiet') or for
-- the next day's budget ('budget'). Delivered by the initiative loop, or
-- moved to the inbox once they're a day old.
CREATE TABLE held_messages (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character  TEXT    NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '', -- job:ID
    title      TEXT    NOT NULL DEFAULT '', -- what it is, for the dashboard
    reason     TEXT    NOT NULL CHECK (reason IN ('quiet', 'budget')),
    text       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX held_messages_user ON held_messages (user_id, character);
