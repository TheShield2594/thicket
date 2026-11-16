-- One person can reach Thicket from several chat platforms (Discord,
-- Telegram) and keep one profile and one set of memories. Each platform
-- account is an identity of a user. users.channel/external_id stay as the
-- identity the user last wrote from: proactive messages go there.
CREATE TABLE identities (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel      TEXT    NOT NULL,
    external_id  TEXT    NOT NULL,
    last_seen_at INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (channel, external_id)
);
CREATE INDEX identities_user ON identities (user_id);
INSERT INTO identities (user_id, channel, external_id, last_seen_at, created_at)
    SELECT id, channel, external_id, last_seen_at, created_at FROM users;
