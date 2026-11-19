-- The message an approval was asked in, when it carries buttons: the id
-- the chat app gave it, in the approval's chat. Once the approval is
-- answered (by a button, by typing, from the dashboard) or expires, the
-- message is edited to show the outcome without its buttons, and this is
-- cleared, so it is settled once.
ALTER TABLE approvals ADD COLUMN prompt_msg TEXT NOT NULL DEFAULT '';
