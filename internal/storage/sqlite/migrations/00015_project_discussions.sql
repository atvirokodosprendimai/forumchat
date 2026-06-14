-- +goose Up
-- +goose StatementBegin

-- Project discussions: forum-style threaded conversations scoped to one
-- project. Members + share-link guests both author. Soft-delete on
-- thread + reply; quoted-reply pointer is a forum.flat-quote shape.

CREATE TABLE project_discussion_threads (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    subject          TEXT NOT NULL,
    body_md          TEXT NOT NULL DEFAULT '',
    body_html        TEXT NOT NULL DEFAULT '',
    creator_user_id  TEXT,
    creator_guest_id TEXT,
    creator_name     TEXT NOT NULL,
    deleted_at       INTEGER,
    last_activity_at INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_project_discussions_project_activity
    ON project_discussion_threads(project_id, last_activity_at DESC);

CREATE TABLE project_discussion_replies (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL REFERENCES project_discussion_threads(id) ON DELETE CASCADE,
    quoted_reply_id TEXT REFERENCES project_discussion_replies(id) ON DELETE SET NULL,
    author_user_id  TEXT,
    author_guest_id TEXT,
    author_name     TEXT NOT NULL,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    edited_at       INTEGER,
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_project_discussion_replies_thread
    ON project_discussion_replies(thread_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_project_discussion_replies_thread;
DROP TABLE IF EXISTS project_discussion_replies;
DROP INDEX IF EXISTS idx_project_discussions_project_activity;
DROP TABLE IF EXISTS project_discussion_threads;

-- +goose StatementEnd
