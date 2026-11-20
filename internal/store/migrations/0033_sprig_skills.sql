-- Sprig-owned skills and skill runs.
-- A skill with a sprig_id belongs to that sprig: only it is offered the
-- skill, and its file in characters/<sprig>/skills/ is the source of
-- truth (mirrored here on load). NULL is the shared library, as before.
ALTER TABLE skills ADD COLUMN sprig_id TEXT;
CREATE INDEX skills_sprig ON skills (sprig_id) WHERE sprig_id IS NOT NULL;
-- Optional fields for taught skills: what the skill needs to be told
-- (a JSON list of {name, description, required}) and how to tell it worked.
ALTER TABLE skill_versions ADD COLUMN inputs TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_versions ADD COLUMN success_check TEXT NOT NULL DEFAULT '';
-- Each time a sprig follows a skill (use_skill), and how it ended.
-- outcome: '' while it runs, then succeeded, failed or stopped (by the
-- person). forced_ask: the run's actions asked first whatever the levels
-- said (a skill's first runs).
CREATE TABLE skill_runs (
    id          INTEGER PRIMARY KEY,
    skill_id    TEXT    NOT NULL,
    version     INTEGER NOT NULL,
    sprig_id    TEXT    NOT NULL,
    user_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    chat_ref    TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    outcome     TEXT    NOT NULL DEFAULT '' CHECK (outcome IN ('', 'succeeded', 'failed', 'stopped')),
    note        TEXT    NOT NULL DEFAULT '',
    forced_ask  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX skill_runs_skill ON skill_runs (skill_id, sprig_id);
CREATE INDEX skill_runs_open ON skill_runs (sprig_id, chat_ref) WHERE outcome = '';
