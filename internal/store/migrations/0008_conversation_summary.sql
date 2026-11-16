-- Long conversations are compacted: older messages are folded into a
-- running summary, and replies see the summary plus the recent messages.
-- summary_upto is the id of the last message the summary covers.
ALTER TABLE conversations ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN summary_upto INTEGER NOT NULL DEFAULT 0;
