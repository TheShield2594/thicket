-- Per-sprig sandboxing: which integrations can run commands, which sprigs
-- may use them, and which paths beyond its own folder a sprig may touch.

-- An integration that can run commands or programs (the sandbox computer,
-- a shell server). Its tools are offered only to sprigs with exec on.
ALTER TABLE mcp_servers ADD COLUMN exec INTEGER NOT NULL DEFAULT 0;

-- Per-sprig opt-in to exec-capable tools. No row means off.
CREATE TABLE sprig_access (
    character  TEXT    PRIMARY KEY,
    exec       INTEGER NOT NULL DEFAULT 0,
    updated_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_at INTEGER NOT NULL
);

-- Paths a sprig may use besides its own folder, each granted by the
-- sprig's owner. Stored resolved (absolute, symlinks followed).
CREATE TABLE path_grants (
    character  TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    granted_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (character, path)
);
