-- Sign-in beyond one-time links: passwords, invites and OpenID Connect,
-- and sessions people can see and revoke.

-- A username and password (argon2id) for a user. password_hash is empty
-- for a login that only signs in with OIDC.
CREATE TABLE logins (
    user_id       INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);

-- Invites from the owner: a one-time link that lets someone choose a
-- username and password (or sign in with OIDC). user_id set: the invite
-- gives an existing user (say, a Discord member) a login.
CREATE TABLE invites (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT    NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    name       TEXT    NOT NULL DEFAULT '',
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    created_at INTEGER NOT NULL
);

-- Accounts at an OpenID Connect provider, by issuer and subject.
CREATE TABLE oidc_identities (
    issuer     TEXT    NOT NULL,
    subject    TEXT    NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX oidc_identities_user ON oidc_identities (user_id);

-- A sign-in in progress at the OIDC provider (10 minutes at most).
CREATE TABLE oidc_states (
    state_hash  TEXT    PRIMARY KEY,
    verifier    TEXT    NOT NULL,
    nonce       TEXT    NOT NULL,
    invite_hash TEXT    NOT NULL DEFAULT '',
    expires_at  INTEGER NOT NULL
);

-- An email address to match OIDC sign-ins to (verified emails only).
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';

-- Sessions get an id that's safe to show (to revoke one), how they
-- signed in, and a hard end however active they stay (0: none, for
-- sessions from before this).
ALTER TABLE web_sessions ADD COLUMN public_id TEXT NOT NULL DEFAULT '';
ALTER TABLE web_sessions ADD COLUMN method TEXT NOT NULL DEFAULT 'link';
ALTER TABLE web_sessions ADD COLUMN max_expires_at INTEGER NOT NULL DEFAULT 0;
UPDATE web_sessions SET public_id = lower(hex(randomblob(8)));
CREATE INDEX web_sessions_public ON web_sessions (public_id);
