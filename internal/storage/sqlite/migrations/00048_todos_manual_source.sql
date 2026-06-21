-- +goose Up
-- Standalone todos: widen source_kind to allow 'manual'. SQLite cannot ALTER a
-- CHECK constraint, so rebuild the table. `todos` is a leaf (no inbound FKs, no
-- triggers), so a copy → drop → rename is safe.

-- +goose StatementBegin
CREATE TABLE todos_new (
    id                TEXT PRIMARY KEY,
    community_id      TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_kind       TEXT NOT NULL CHECK (source_kind IN ('chat','forum_post','manual')),
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
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO todos_new
SELECT id, community_id, user_id, source_kind, source_id, source_thread_id, source_day,
       title, body_snapshot, category, note, status, created_at, updated_at, completed_at
FROM todos;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE todos;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE todos_new RENAME TO todos;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_todos_user_status_created ON todos(user_id, community_id, status, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_todos_user_category ON todos(user_id, community_id, category);
-- +goose StatementEnd


-- +goose Down
-- Revert to the chat/forum-only CHECK. Drop any manual rows first so the
-- narrower constraint can't be violated by the copy.

-- +goose StatementBegin
DELETE FROM todos WHERE source_kind = 'manual';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE todos_old (
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
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO todos_old
SELECT id, community_id, user_id, source_kind, source_id, source_thread_id, source_day,
       title, body_snapshot, category, note, status, created_at, updated_at, completed_at
FROM todos;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE todos;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE todos_old RENAME TO todos;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_todos_user_status_created ON todos(user_id, community_id, status, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_todos_user_category ON todos(user_id, community_id, category);
-- +goose StatementEnd
