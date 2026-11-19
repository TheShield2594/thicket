-- Inviting another adult to make a sprig of their own ("add my partner's
-- sprig"), and a short tour for people who join later.

-- An invite can ask for the person to be helped to their own private sprig
-- as soon as they arrive. It's kept on the user until their tour ends.
ALTER TABLE invites ADD COLUMN own_sprig INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN own_sprig INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN invited_by INTEGER REFERENCES users (id) ON DELETE SET NULL;

-- People who joined before the tour existed don't get it now.
UPDATE users SET onboarded_at = created_at WHERE onboarded_at IS NULL AND role <> 'owner' AND role <> 'admin';
