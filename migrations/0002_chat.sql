-- +goose Up
-- SQL in this section is executed when the migration is applied.

CREATE TABLE IF NOT EXISTS chat (
    id SERIAL PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS chat_user (
    id SERIAL PRIMARY KEY,
    handle VARCHAR(45) NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_line (
    id BIGSERIAL PRIMARY KEY,
    chat_id INT NOT NULL,
    user_id INT NOT NULL,
    line_text TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    summary TEXT,
    CONSTRAINT fk_chat_line_chat FOREIGN KEY (chat_id) REFERENCES chat(id) ON DELETE NO ACTION ON UPDATE NO ACTION,
    CONSTRAINT fk_chat_line_chat_user FOREIGN KEY (user_id) REFERENCES chat_user(id) ON DELETE NO ACTION ON UPDATE NO ACTION
);

CREATE INDEX IF NOT EXISTS idx_chat_line_chat ON chat_line(chat_id);
CREATE INDEX IF NOT EXISTS idx_chat_line_user ON chat_line(user_id);


-- +goose Down
-- SQL in this section is executed when the migration is rolled back.

DROP TABLE IF EXISTS chat_line;
DROP TABLE IF EXISTS chat_user;
DROP TABLE IF EXISTS chat;
