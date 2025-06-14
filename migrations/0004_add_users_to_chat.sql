-- +goose Up
-- +goose StatementBegin

-- Add more fields to chat table for better chat management
ALTER TABLE chat ADD COLUMN IF NOT EXISTS title VARCHAR(255) DEFAULT 'New Chat';
ALTER TABLE chat ADD COLUMN IF NOT EXISTS created_by INT REFERENCES chat_user(id);
ALTER TABLE chat ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT NOW();
ALTER TABLE chat ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT NOW();
ALTER TABLE chat ADD COLUMN IF NOT EXISTS is_active BOOLEAN DEFAULT true;

-- Create a junction table for chat participants (many-to-many relationship)
CREATE TABLE IF NOT EXISTS chat_participants (
    id SERIAL PRIMARY KEY,
    chat_id INT NOT NULL REFERENCES chat(id) ON DELETE CASCADE,
    user_id INT NOT NULL REFERENCES chat_user(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ DEFAULT NOW(),
    role VARCHAR(20) DEFAULT 'member', -- 'owner', 'member', 'admin'
    -- change default to owner
    UNIQUE(chat_id, user_id)
);

-- Add indexes for better performance
CREATE INDEX IF NOT EXISTS idx_chat_participants_chat ON chat_participants(chat_id);
CREATE INDEX IF NOT EXISTS idx_chat_participants_user ON chat_participants(user_id);
CREATE INDEX IF NOT EXISTS idx_chat_created_by ON chat(created_by);
CREATE INDEX IF NOT EXISTS idx_chat_updated_at ON chat(updated_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chat_participants;
ALTER TABLE chat DROP COLUMN IF EXISTS title;
ALTER TABLE chat DROP COLUMN IF EXISTS created_by;
ALTER TABLE chat DROP COLUMN IF EXISTS created_at;
ALTER TABLE chat DROP COLUMN IF EXISTS updated_at;
ALTER TABLE chat DROP COLUMN IF EXISTS is_active;
-- +goose StatementEnd
