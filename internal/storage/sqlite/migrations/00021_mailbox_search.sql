-- +goose Up
-- +goose StatementBegin

-- email body persistence + full-text search.
--
-- body_text on email_ingest captures the text/plain (or html→text) body
-- at poll time so search hits the local DB rather than IMAP.
-- email_ingest_fts is the FTS5 index over the searchable fields plus a
-- denormalised concatenation of the message's attachment filenames so
-- a query like "api documentation" matches both bodies that mention it
-- AND attachments named "api documentation.doc".
--
-- The app layer is responsible for keeping email_ingest_fts in sync
-- (no triggers — they tangle the test setup and we already have a
-- single-writer transactional ingest path that can manage one extra
-- INSERT/UPDATE).

ALTER TABLE email_ingest ADD COLUMN body_text TEXT NOT NULL DEFAULT '';

CREATE VIRTUAL TABLE email_ingest_fts USING fts5(
    ingest_id        UNINDEXED,
    subject,
    from_addr,
    from_name,
    body_text,
    attachment_names,
    tokenize='porter'
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS email_ingest_fts;
ALTER TABLE email_ingest DROP COLUMN body_text;

-- +goose StatementEnd
