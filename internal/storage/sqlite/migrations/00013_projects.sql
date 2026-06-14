-- +goose Up
-- +goose StatementBegin

-- Projects are per-community collaborative pages with title, description,
-- a project-local checklist, drag-drop document attachments, and an inline
-- comment thread. Feature is gated by PROJECTS_ENABLED env at the route
-- mount level, but the tables always exist so toggling the flag never
-- needs a schema migration.

CREATE TABLE projects (
    id               TEXT PRIMARY KEY,
    community_id     TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    creator_user_id  TEXT NOT NULL REFERENCES users(id),
    title            TEXT NOT NULL,
    description_md   TEXT NOT NULL DEFAULT '',
    description_html TEXT NOT NULL DEFAULT '',
    archived_at      INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_projects_community ON projects(community_id, updated_at DESC);

CREATE TABLE project_todos (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    body         TEXT NOT NULL,
    done         INTEGER NOT NULL DEFAULT 0,
    sort_order   INTEGER NOT NULL,
    created_by   TEXT NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_project_todos_project ON project_todos(project_id, sort_order);

CREATE TABLE project_attachments (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    upload_id    TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    filename     TEXT NOT NULL,
    mime         TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    uploader_id  TEXT NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_project_attachments_project ON project_attachments(project_id, created_at DESC);

CREATE TABLE project_comments (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author_id   TEXT NOT NULL REFERENCES users(id),
    body_md     TEXT NOT NULL,
    body_html   TEXT NOT NULL,
    edited_at   INTEGER,
    deleted_at  INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_project_comments_project ON project_comments(project_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_project_comments_project;
DROP TABLE IF EXISTS project_comments;
DROP INDEX IF EXISTS idx_project_attachments_project;
DROP TABLE IF EXISTS project_attachments;
DROP INDEX IF EXISTS idx_project_todos_project;
DROP TABLE IF EXISTS project_todos;
DROP INDEX IF EXISTS idx_projects_community;
DROP TABLE IF EXISTS projects;

-- +goose StatementEnd
