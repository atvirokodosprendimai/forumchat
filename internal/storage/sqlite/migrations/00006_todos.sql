-- +goose Up
-- +goose StatementBegin

ALTER TABLE communities ADD COLUMN todos_enabled INTEGER NOT NULL DEFAULT 0;

CREATE TABLE todos (
    id                TEXT PRIMARY KEY,
    community_id      TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_kind       TEXT NOT NULL CHECK (source_kind IN ('chat','forum_post')),
    source_id         TEXT NOT NULL,
    source_thread_id  TEXT,
    source_day        TEXT,
    title             TEXT NOT NULL,
    body_snapshot     TEXT NOT NULL DEFAULT '',
    category          TEXT NOT NULL DEFAULT '',
    note              TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','doing','done')),
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    completed_at      INTEGER
);
CREATE INDEX idx_todos_user_status_created ON todos(user_id, community_id, status, created_at);
CREATE INDEX idx_todos_user_category       ON todos(user_id, community_id, category);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_todos_user_category;
DROP INDEX IF EXISTS idx_todos_user_status_created;
DROP TABLE IF EXISTS todos;
ALTER TABLE communities DROP COLUMN todos_enabled;

-- +goose StatementEnd
