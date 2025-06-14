-- +goose Up
-- +goose StatementBegin
ALTER TABLE chat
ADD COLUMN IF NOT EXISTS title VARCHAR(255) NOT NULL DEFAULT 'New Chat';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Your original Down migration was already idempotent using IF EXISTS,
-- which is perfect. No changes are needed here.
ALTER TABLE chat
DROP COLUMN IF EXISTS title;
-- +goose StatementEnd
