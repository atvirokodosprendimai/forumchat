-- +goose Up
-- chat_message_attachments links N upload rows to one chat message so
-- a single bubble can carry multiple files (one image + one pdf + one
-- video, etc). ON DELETE CASCADE so soft-delete of the message clears
-- the link rows (the upload itself is reference-counted elsewhere and
-- only gets pruned when no row anywhere references its rel_path).
CREATE TABLE chat_message_attachments (
    id              TEXT PRIMARY KEY,
    chat_message_id TEXT NOT NULL,
    upload_id       TEXT NOT NULL,
    position        INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    FOREIGN KEY (chat_message_id) REFERENCES chat_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (upload_id)       REFERENCES uploads(id)
);
CREATE INDEX idx_chat_msg_atts_msg
    ON chat_message_attachments (chat_message_id, position);
CREATE INDEX idx_chat_msg_atts_upload
    ON chat_message_attachments (upload_id);

-- +goose Down
DROP TABLE chat_message_attachments;
