-- +goose Up
-- +goose StatementBegin

-- forwarded_from_msg_id references the original chat_messages row this
-- message was forwarded from (Discord-style "Forwarded from #channel").
-- Soft reference like reply_to_id / promoted_thread_id: the render path
-- LEFT JOINs it and tolerates a NULL (source hard-deleted). The source
-- can live in any channel — forwarding is explicitly cross-channel.
ALTER TABLE chat_messages ADD COLUMN forwarded_from_msg_id TEXT REFERENCES chat_messages(id) ON DELETE SET NULL;
CREATE INDEX idx_chat_messages_forwarded_from ON chat_messages(forwarded_from_msg_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_chat_messages_forwarded_from;
ALTER TABLE chat_messages DROP COLUMN forwarded_from_msg_id;
-- +goose StatementEnd
