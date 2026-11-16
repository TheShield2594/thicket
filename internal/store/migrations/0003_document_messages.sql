-- Messages gain a 'document' role: text read from a PDF the user sent,
-- kept so later turns of the conversation can answer follow-up questions.
-- SQLite can't change a CHECK constraint in place, so the table is rebuilt.
CREATE TABLE messages_new (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'assistant', 'event', 'document')),
    character       TEXT    NOT NULL DEFAULT '',
    content         TEXT    NOT NULL,
    created_at      INTEGER NOT NULL
);
INSERT INTO messages_new (id, conversation_id, role, character, content, created_at)
    SELECT id, conversation_id, role, character, content, created_at FROM messages;
DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;
CREATE INDEX messages_conv ON messages (conversation_id, id);
