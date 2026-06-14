-- +goose Up
-- +goose StatementBegin

-- Project issues: lighter than forum threads, heavier than chat. Each
-- project has its own list. Statuses: open/triaged/in_progress/closed.
-- Guests may also author (creator_user_id NULL + creator_guest_id set);
-- the guest table itself lives below.

CREATE TABLE project_issues (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title            TEXT NOT NULL,
    body_md          TEXT NOT NULL DEFAULT '',
    body_html        TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'open'
                     CHECK (status IN ('open','triaged','in_progress','closed')),
    creator_user_id  TEXT,
    creator_guest_id TEXT,
    creator_name     TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_project_issues_project_status
    ON project_issues(project_id, status, updated_at DESC);

CREATE TABLE project_issue_comments (
    id               TEXT PRIMARY KEY,
    issue_id         TEXT NOT NULL REFERENCES project_issues(id) ON DELETE CASCADE,
    author_user_id   TEXT,
    author_guest_id  TEXT,
    author_name      TEXT NOT NULL,
    body_md          TEXT NOT NULL,
    body_html        TEXT NOT NULL,
    edited_at        INTEGER,
    deleted_at       INTEGER,
    created_at       INTEGER NOT NULL
);
CREATE INDEX idx_project_issue_comments_issue
    ON project_issue_comments(issue_id, created_at);

CREATE TABLE project_issue_attachments (
    id                TEXT PRIMARY KEY,
    issue_id          TEXT NOT NULL REFERENCES project_issues(id) ON DELETE CASCADE,
    comment_id        TEXT REFERENCES project_issue_comments(id) ON DELETE CASCADE,
    upload_id         TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    uploader_user_id  TEXT,
    uploader_guest_id TEXT,
    uploader_name     TEXT NOT NULL,
    created_at        INTEGER NOT NULL
);
CREATE INDEX idx_project_issue_attachments_issue
    ON project_issue_attachments(issue_id, created_at);

-- Guest share links — TTL-scoped, one active per project at a time.
CREATE TABLE project_guest_invites (
    token       TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_by  TEXT NOT NULL REFERENCES users(id),
    expires_at  INTEGER,
    revoked_at  INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_project_guest_invites_project ON project_guest_invites(project_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_project_guest_invites_project;
DROP TABLE IF EXISTS project_guest_invites;
DROP INDEX IF EXISTS idx_project_issue_attachments_issue;
DROP TABLE IF EXISTS project_issue_attachments;
DROP INDEX IF EXISTS idx_project_issue_comments_issue;
DROP TABLE IF EXISTS project_issue_comments;
DROP INDEX IF EXISTS idx_project_issues_project_status;
DROP TABLE IF EXISTS project_issues;

-- +goose StatementEnd
