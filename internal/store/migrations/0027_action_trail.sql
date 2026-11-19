-- Act-level actions in the trail, said in the chat with how to undo them.

-- What reverses an integration's tools: {"create_item": {"tool":
-- "delete_item", "args": {"id": "{{result.id}}"}, "said": "I added
-- {{args.name}} to {{args.list}}", "label": "remove it"}}.
ALTER TABLE mcp_servers ADD COLUMN undo TEXT NOT NULL DEFAULT '{}';

-- The chat app's ids for the line saying what was done, so "undo" in
-- reply to it finds the trail entry.
CREATE TABLE trail_acks (
    chat_ref    TEXT    NOT NULL,
    platform_id TEXT    NOT NULL,
    trail_id    INTEGER NOT NULL REFERENCES trail (id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (chat_ref, platform_id)
);
CREATE INDEX trail_acks_recent ON trail_acks (chat_ref, user_id, created_at);

-- The cost estimate an approval was asked with, so approving it is checked
-- and measured against the figure that was shown.
ALTER TABLE approvals ADD COLUMN estimate_id INTEGER NOT NULL DEFAULT 0;
