-- +goose Up
-- +goose StatementBegin

-- Persisted HTML body for the inbox-detail rendering. We already store
-- the plaintext (body_text) for search + plain fallback. The HTML
-- column is sanitized at the app layer via bluemonday at view time
-- (raw IMAP HTML lands here verbatim — sanitization is read-side so a
-- tighter policy can be deployed without re-fetching).

ALTER TABLE email_ingest ADD COLUMN body_html TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE email_ingest DROP COLUMN body_html;

-- +goose StatementEnd
