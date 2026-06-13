-- +goose Up
-- +goose StatementBegin

ALTER TABLE chat_messages ADD COLUMN promoted_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL;
CREATE INDEX idx_chat_messages_promoted ON chat_messages(promoted_thread_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_chat_messages_promoted;
ALTER TABLE chat_messages DROP COLUMN promoted_thread_id;

-- +goose StatementEnd
