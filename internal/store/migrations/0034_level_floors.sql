-- Level floors (Phase 4): why an approval always asks whatever the
-- sprig's permission level ("" = it isn't floored), e.g. "payment page"
-- or "run 1 of 3 of a new skill", shown in "needs you".
ALTER TABLE approvals ADD COLUMN floor TEXT NOT NULL DEFAULT '';
