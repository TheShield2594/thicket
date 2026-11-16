-- What each chat call's prompt was made of, in characters, so reports can
-- say how much of chat spend went on conversation history and on PDF text
-- kept for follow-ups. 0 for calls that weren't measured.
ALTER TABLE usage ADD COLUMN prompt_chars INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN history_chars INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN document_chars INTEGER NOT NULL DEFAULT 0;
