-- +goose Up
-- +goose StatementBegin
ALTER TABLE chat
ADD COLUMN title VARCHAR(255) NOT NULL DEFAULT 'New Chat';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE chat
DROP COLUMN IF EXISTS title;
-- +goose StatementEnd