-- +goose Up
-- Drop the existing foreign key constraint which has "ON DELETE NO ACTION"
ALTER TABLE chat_line
DROP CONSTRAINT fk_chat_line_chat;

-- Add a new foreign key constraint with "ON DELETE CASCADE"
ALTER TABLE chat_line
ADD CONSTRAINT fk_chat_line_chat
FOREIGN KEY (chat_id) REFERENCES chat(id)
ON DELETE CASCADE;

-- +goose Down
-- This section reverts the changes if you ever need to roll back the migration.

-- Drop the cascading constraint
ALTER TABLE chat_line
DROP CONSTRAINT fk_chat_line_chat;

-- Re-add the original, non-cascading constraint
ALTER TABLE chat_line
ADD CONSTRAINT fk_chat_line_chat
FOREIGN KEY (chat_id) REFERENCES chat(id)
ON DELETE NO ACTION ON UPDATE NO ACTION;
