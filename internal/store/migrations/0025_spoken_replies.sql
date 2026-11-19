-- Spoken replies: how each person likes each sprig to talk back, and the
-- audio of spoken replies in the web app.

-- mode: brief (a short spoken summary, the full text alongside), full
-- (the whole reply read aloud) or off. No row means full.
CREATE TABLE speech_prefs (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character  TEXT    NOT NULL,
    mode       TEXT    NOT NULL CHECK (mode IN ('brief', 'full', 'off')),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, character)
);

-- A spoken reply in the web app is the audio of one of its messages.
CREATE TABLE web_audio (
    message_id   INTEGER PRIMARY KEY REFERENCES web_messages (id) ON DELETE CASCADE,
    content_type TEXT    NOT NULL,
    data         BLOB    NOT NULL
);
