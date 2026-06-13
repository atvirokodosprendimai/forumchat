-- +goose Up
-- +goose StatementBegin

CREATE TABLE bookmarks (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    chat_message_id TEXT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    title           TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    note            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    UNIQUE (user_id, chat_message_id)
);
CREATE INDEX idx_bookmarks_user_created ON bookmarks(user_id, created_at);
CREATE INDEX idx_bookmarks_user_category ON bookmarks(user_id, category);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bookmarks;
-- +goose StatementEnd
