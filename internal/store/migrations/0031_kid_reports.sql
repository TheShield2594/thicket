-- How a parent wants the report on the kid sprigs they look after: daily,
-- weekly (in the weekly digest, the default) or off.
ALTER TABLE digest_prefs ADD COLUMN kids TEXT NOT NULL DEFAULT 'weekly' CHECK (kids IN ('daily', 'weekly', 'off'));
