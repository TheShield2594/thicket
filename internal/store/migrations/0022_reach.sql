-- Where proactive messages reach a person: character '' is their choice
-- for every sprig ("reach me on Telegram"), a sprig id overrides it for
-- that sprig. No row means the default: the chat app they last used,
-- preferring a phone app (Telegram, Discord) over the web app.
CREATE TABLE reach_prefs (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character  TEXT    NOT NULL DEFAULT '',
    channel    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, character)
);
