-- +goose Up
-- +goose StatementBegin
-- Use IF NOT EXISTS to prevent errors if the columns already exist.
-- This is the key change to fix the original error.
ALTER TABLE chat
ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
-- +goose StatementEnd

-- +goose StatementBegin
-- CREATE OR REPLACE FUNCTION is naturally idempotent. It will either create the
-- function or replace it if it already exists.
CREATE OR REPLACE FUNCTION trigger_set_timestamp()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
-- To make trigger creation idempotent, we wrap it in a DO block and check
-- the pg_trigger catalog to see if a trigger with that name already exists.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger
    WHERE tgname = 'set_chat_updated_at' AND tgrelid = 'chat'::regclass
  ) THEN
    CREATE TRIGGER set_chat_updated_at
    BEFORE UPDATE ON chat
    FOR EACH ROW
    EXECUTE FUNCTION trigger_set_timestamp();
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- The Down migration uses IF EXISTS to ensure it can run without errors
-- even if the objects have already been removed.
-- +goose StatementBegin
-- Drop the trigger if it exists on the chat table.
DROP TRIGGER IF EXISTS set_chat_updated_at ON chat;
-- +goose StatementEnd

-- +goose StatementBegin
-- Note: We don't drop the trigger_set_timestamp() function here, as other
-- tables might be using it. It's generally safe to leave helper functions.
-- Drop the columns if they exist.
ALTER TABLE chat
DROP COLUMN IF EXISTS updated_at,
DROP COLUMN IF EXISTS created_at;
-- +goose StatementEnd
