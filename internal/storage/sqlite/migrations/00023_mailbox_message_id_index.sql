-- +goose Up
-- +goose StatementBegin

-- Speeds up findIngestByMessageID dedup probe added 2026-06-15.
-- Partial index — empty message_id is common for autoreplies and
-- we don't care about indexing those (the dedup path skips them).

CREATE INDEX IF NOT EXISTS idx_email_ingest_message_id
    ON email_ingest(message_id)
    WHERE message_id <> '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_email_ingest_message_id;

-- +goose StatementEnd
