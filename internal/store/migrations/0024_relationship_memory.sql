-- Relationship memory and the memory screen.

-- Where a memory came from: "review" (the automatic memory review),
-- "sprig:<id>" (a sprig saved it in chat), "person:<user id>" (someone
-- added it on the memory screen), "import", or '' (before this was kept).
ALTER TABLE memories ADD COLUMN origin TEXT NOT NULL DEFAULT '';

-- Who made a change on the memory screen (NULL for sprigs and the review),
-- and when it was undone, so person edits can be undone one by one.
ALTER TABLE memory_events ADD COLUMN by_user INTEGER;
ALTER TABLE memory_events ADD COLUMN undone_at INTEGER;
CREATE INDEX memory_events_memory ON memory_events (memory_id);
