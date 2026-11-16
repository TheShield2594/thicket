-- Web app: sign-in links, sessions, and the transcript of web chats.

-- One-time sign-in links. Only a hash of the token is stored.
CREATE TABLE web_links (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER REFERENCES users (id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    created_at INTEGER NOT NULL
);

CREATE TABLE web_sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    user_agent   TEXT    NOT NULL DEFAULT '',
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX web_sessions_user ON web_sessions (user_id);

-- What the user saw in a web chat, exactly as delivered. chat_id is
-- "<user id>.<character id>" or "<user id>.all" for the shared room.
CREATE TABLE web_messages (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chat_id    TEXT    NOT NULL,
    role       TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
    character  TEXT    NOT NULL DEFAULT '',
    content    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX web_messages_chat ON web_messages (chat_id, id);
