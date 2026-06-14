-- +goose Up

-- Attachment categories. Free text but the UI suggests a small set
-- (common, api_docs, design, other). DEFAULT 'common' backfills every
-- existing row at the moment the column is added — that's the
-- migration the user explicitly asked for.
ALTER TABLE project_attachments ADD COLUMN category TEXT NOT NULL DEFAULT 'common';
CREATE INDEX idx_project_attachments_category
    ON project_attachments(project_id, category);

-- Per-community "what's changed in projects since the last digest"
-- cursor. The chat-digest worker bumps last_at after each successful
-- post; first-time encounter initialises to now() so we never spam
-- chat with the entire history.
CREATE TABLE project_chat_digest_state (
    community_id TEXT PRIMARY KEY,
    last_at      INTEGER NOT NULL,  -- unix millis
    updated_at   INTEGER NOT NULL   -- unix millis (when worker last touched)
);

-- +goose Down
DROP TABLE IF EXISTS project_chat_digest_state;
DROP INDEX IF EXISTS idx_project_attachments_category;
ALTER TABLE project_attachments DROP COLUMN category;
