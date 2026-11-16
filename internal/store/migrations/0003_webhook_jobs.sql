-- Jobs can be started by a webhook instead of a cron schedule. Only a
-- SHA-256 hash of the webhook's token is stored: the URL is shown to the
-- user once and can be regenerated, never read back.
ALTER TABLE jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'cron' CHECK (kind IN ('cron', 'webhook'));
ALTER TABLE jobs ADD COLUMN hook_hash TEXT;
CREATE UNIQUE INDEX jobs_hook ON jobs (hook_hash) WHERE hook_hash IS NOT NULL;
