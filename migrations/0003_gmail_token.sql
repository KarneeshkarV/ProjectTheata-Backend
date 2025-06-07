-- +goose Up
-- +goose NO TRANSACTION
-- +goose StatementBegin
ALTER TABLE users 
ADD COLUMN gmail_auth_token TEXT UNIQUE,
ADD COLUMN gmail_refresh_token TEXT,
ADD COLUMN gmail_token_expires_at TIMESTAMPTZ,
ADD COLUMN gmail_connected_at TIMESTAMPTZ;
-- +goose StatementEnd

-- +goose StatementBegin
-- Create index for efficient lookups by auth token
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_users_gmail_auth_token 
ON users(gmail_auth_token) WHERE gmail_auth_token IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Update the updated_at trigger to include new columns
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';
-- +goose StatementEnd

-- +goose StatementBegin
-- Ensure trigger exists for users table
DROP TRIGGER IF EXISTS update_users_updated_at ON users;
CREATE TRIGGER update_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Drop the index first
DROP INDEX IF EXISTS idx_users_gmail_auth_token;

-- Remove the Gmail-related columns
ALTER TABLE users 
DROP COLUMN IF EXISTS gmail_connected_at,
DROP COLUMN IF EXISTS gmail_token_expires_at,
DROP COLUMN IF EXISTS gmail_refresh_token,
DROP COLUMN IF EXISTS gmail_auth_token;
-- +goose StatementEnd
