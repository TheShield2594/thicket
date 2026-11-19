-- The weekly digest: how each adult wants it. mode: always (a quiet week
-- still gets one line), skip_empty (a quiet week sends nothing) or off.
-- No row means always. The day and time are household settings
-- (digest.day, digest.time), in each person's own timezone.
CREATE TABLE digest_prefs (
    user_id    INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    mode       TEXT    NOT NULL CHECK (mode IN ('always', 'skip_empty', 'off')),
    updated_at INTEGER NOT NULL
);
