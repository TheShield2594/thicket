-- What an API token is for, enforced by the server: cli (scripts and the
-- local API, made with `thicket api-token create`), mcp (an AI agent
-- through `thicket mcp`: no approvals, no admin) or app (signed in through
-- POST /api/v2/auth/login). Existing tokens: the apps' are named "app: …".
ALTER TABLE api_tokens ADD COLUMN kind TEXT NOT NULL DEFAULT 'cli' CHECK (kind IN ('cli', 'mcp', 'app'));
UPDATE api_tokens SET kind = 'app' WHERE substr(name, 1, 5) = 'app: ';
