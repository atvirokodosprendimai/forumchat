-- +goose Up
-- +goose StatementBegin

-- Rooms moved from a single global pool to per-community pools so that
-- each community has its own 8 meeting slots. We drop and recreate the
-- four tables because SQLite can't ALTER COLUMN to add NOT NULL +
-- composite unique on existing data — and the rooms feature is new
-- enough (00011) that we don't need to preserve in-flight content.

DROP INDEX IF EXISTS idx_room_invites_room;
DROP TABLE IF EXISTS room_invites;
DROP INDEX IF EXISTS idx_room_chat_room;
DROP TABLE IF EXISTS room_chat;
DROP TABLE IF EXISTS rooms;

CREATE TABLE rooms (
    id              TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    slot            INTEGER NOT NULL CHECK (slot BETWEEN 1 AND 8),
    name            TEXT NOT NULL,
    is_public       INTEGER NOT NULL DEFAULT 0,
    admin_user_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (community_id, slot)
);
CREATE INDEX idx_rooms_community ON rooms(community_id);

CREATE TABLE room_chat (
    id              TEXT PRIMARY KEY,
    room_id         TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    author_user_id  TEXT,
    author_name     TEXT NOT NULL,
    body            TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_room_chat_room ON room_chat(room_id, created_at);
CREATE INDEX idx_room_chat_community ON room_chat(community_id, created_at);

CREATE TABLE room_invites (
    token         TEXT PRIMARY KEY,
    room_id       TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    created_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,
    revoked_at    INTEGER
);
CREATE INDEX idx_room_invites_room ON room_invites(room_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_room_invites_room;
DROP TABLE IF EXISTS room_invites;
DROP INDEX IF EXISTS idx_room_chat_community;
DROP INDEX IF EXISTS idx_room_chat_room;
DROP TABLE IF EXISTS room_chat;
DROP INDEX IF EXISTS idx_rooms_community;
DROP TABLE IF EXISTS rooms;

-- +goose StatementEnd
