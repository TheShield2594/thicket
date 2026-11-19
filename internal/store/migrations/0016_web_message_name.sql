-- The name a web message was sent under, when it isn't simply the
-- character's: a preview of a sprig that doesn't exist yet ("Pippa
-- (preview)") has no roster entry for the app to look its name up in.
ALTER TABLE web_messages ADD COLUMN name TEXT NOT NULL DEFAULT '';
