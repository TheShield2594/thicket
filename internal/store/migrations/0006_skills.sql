-- Skills: named procedures any sprig can follow ("how I like the morning
-- briefing done"). Every change is a new version; nothing is overwritten.
CREATE TABLE skills (
    id         TEXT    PRIMARY KEY,
    version    INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE skill_versions (
    skill_id    TEXT    NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL,
    body        TEXT    NOT NULL,
    note        TEXT    NOT NULL DEFAULT '',
    author      TEXT    NOT NULL DEFAULT '', -- sprig id, or "import"
    user_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (skill_id, version)
);
