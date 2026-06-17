-- +goose Up
-- chat_attachment_extracts tracks "this chat attachment was filed into
-- a project" — either as a Docs entry (project_attachments) or as a
-- new issue (project_issues + project_issue_attachments). The link is
-- one-way: extracting NEVER mutates the chat row. Multiple extracts of
-- the same chat attachment are allowed (file in two projects, file in
-- Docs AND open an issue from it). The "↗ in X" badge on the bubble
-- reads from here.
CREATE TABLE chat_attachment_extracts (
    id                    TEXT PRIMARY KEY,
    chat_attachment_id    TEXT NOT NULL,
    project_id            TEXT NOT NULL,
    project_attachment_id TEXT NOT NULL DEFAULT '',
    issue_id              TEXT NOT NULL DEFAULT '',
    mode                  TEXT NOT NULL,  -- 'docs' | 'issue'
    extracted_by          TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    FOREIGN KEY (chat_attachment_id) REFERENCES chat_message_attachments(id) ON DELETE CASCADE
);
CREATE INDEX idx_chat_att_extracts_att     ON chat_attachment_extracts (chat_attachment_id, created_at);
CREATE INDEX idx_chat_att_extracts_project ON chat_attachment_extracts (project_id);

-- +goose Down
DROP TABLE chat_attachment_extracts;
