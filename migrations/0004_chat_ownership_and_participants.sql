-- +goose Up
-- Add a column to the chat table to track who created it
ALTER TABLE chat
ADD COLUMN IF NOT EXISTS created_by_user_id UUID REFERENCES auth.users(id) ON DELETE SET NULL;

-- Create a table to manage who is a member of which chat (many-to-many relationship)
CREATE TABLE IF NOT EXISTS chat_participants (
    chat_id UUID NOT NULL REFERENCES chat(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    role VARCHAR(20) NOT NULL DEFAULT 'member', -- e.g., 'owner', 'member'
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, user_id) -- A user can only be in a chat once
);

CREATE INDEX IF NOT EXISTS idx_chat_participants_chat_id ON chat_participants(chat_id);
CREATE INDEX IF NOT EXISTS idx_chat_participants_user_id ON chat_participants(user_id);

-- +goose Down
DROP TABLE IF EXISTS chat_participants;
ALTER TABLE chat
DROP COLUMN IF EXISTS created_by_user_id;
