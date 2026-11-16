-- Webhook jobs can require the sender to sign each call: '' (the URL is
-- the only secret), 'github', 'gitea' (also Forgejo) or 'gitlab'. The
-- signing secret is kept in the encrypted secret store.
ALTER TABLE jobs ADD COLUMN hook_auth TEXT NOT NULL DEFAULT '' CHECK (hook_auth IN ('', 'github', 'gitea', 'gitlab'));
