-- Sign-ins for the read-only dashboard. A 'login' row is a one-time link
-- (10 minutes); using it replaces it with a 'session' row (the browser's
-- cookie, 7 days). Only SHA-256 hashes of the tokens are stored.
CREATE TABLE web_sessions (
    hash       TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('login', 'session')),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
