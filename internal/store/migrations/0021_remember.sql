-- "remember this": the sprig's one-line confirmation is recorded against
-- the memory it made, so "forget this" in reply to it undoes it.
CREATE TABLE remember_acks (
    chat_ref    TEXT    NOT NULL,
    platform_id TEXT    NOT NULL,
    memory_id   INTEGER NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (chat_ref, platform_id)
);
CREATE INDEX remember_acks_memory ON remember_acks (memory_id);

-- Where "remember this" in a family sprig chat goes, once the person has
-- answered "just for you, or for the family?": '' (not asked or no
-- answer: private), 'private' or 'family'.
ALTER TABLE users ADD COLUMN remember_scope TEXT NOT NULL DEFAULT '';
