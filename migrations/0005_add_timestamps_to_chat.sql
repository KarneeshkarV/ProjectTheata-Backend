-- +goose Up
-- +goose StatementBegin
-- Add missing timestamp columns to the chat table
ALTER TABLE chat
ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
-- +goose StatementEnd

-- +goose StatementBegin
-- Create or replace the function to update the 'updated_at' timestamp.
-- Using CREATE OR REPLACE is safe and ensures it exists for our trigger.
CREATE OR REPLACE FUNCTION trigger_set_timestamp()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
-- Create the trigger for the chat table to automatically update 'updated_at' on any change.
CREATE TRIGGER set_chat_updated_at
BEFORE UPDATE ON chat
FOR EACH ROW
EXECUTE FUNCTION trigger_set_timestamp();
-- +goose StatementEnd


-- +goose Down
-- In reverse order, remove the trigger and then the columns.
-- +goose StatementBegin
DROP TRIGGER IF EXISTS set_chat_updated_at ON chat;
-- +goose StatementEnd

-- +goose StatementBegin
-- Note: We don't drop the function here, as other tables (like user_google_tokens) might be using it.
-- It's generally safe to leave helper functions like this in place.

ALTER TABLE chat
DROP COLUMN IF EXISTS updated_at,
DROP COLUMN IF EXISTS created_at;
-- +goose StatementEnd