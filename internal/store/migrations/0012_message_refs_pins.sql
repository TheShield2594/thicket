-- Pinning and deleting messages.
-- A pinned message stays in every reply's context for the rest of its
-- conversation, even after older messages are folded into the summary.
ALTER TABLE messages ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

-- The chat app's ids for a stored message, so a message deleted (or
-- replied to) in Discord, Telegram or the web app can be found again. A
-- long sprig reply can be posted as several chat messages.
CREATE TABLE message_refs (
    chat_ref    TEXT    NOT NULL,
    platform_id TEXT    NOT NULL,
    message_id  INTEGER NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    PRIMARY KEY (chat_ref, platform_id)
);
CREATE INDEX message_refs_message ON message_refs (message_id);
