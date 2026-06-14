-- +goose Up
-- +goose StatementBegin

CREATE TABLE rooms (
    id              TEXT PRIMARY KEY,
    slot            INTEGER NOT NULL UNIQUE CHECK (slot BETWEEN 1 AND 8),
    name            TEXT NOT NULL,
    is_public       INTEGER NOT NULL DEFAULT 0,
    admin_user_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE room_chat (
    id              TEXT PRIMARY KEY,
    room_id         TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    author_user_id  TEXT,                 -- NULL when posted by a guest
    author_name     TEXT NOT NULL,        -- snapshot (display name or guest name)
    body            TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_room_chat_room ON room_chat(room_id, created_at);

CREATE TABLE room_invites (
    token         TEXT PRIMARY KEY,
    room_id       TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    created_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,
    revoked_at    INTEGER
);
CREATE INDEX idx_room_invites_room ON room_invites(room_id);

INSERT INTO rooms (id, slot, name, is_public, admin_user_id, created_at, updated_at) VALUES
  ('room-01', 1, 'Room 1', 0, NULL, 0, 0),
  ('room-02', 2, 'Room 2', 0, NULL, 0, 0),
  ('room-03', 3, 'Room 3', 0, NULL, 0, 0),
  ('room-04', 4, 'Room 4', 0, NULL, 0, 0),
  ('room-05', 5, 'Room 5', 0, NULL, 0, 0),
  ('room-06', 6, 'Room 6', 0, NULL, 0, 0),
  ('room-07', 7, 'Room 7', 0, NULL, 0, 0),
  ('room-08', 8, 'Room 8', 0, NULL, 0, 0);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_room_invites_room;
DROP TABLE IF EXISTS room_invites;
DROP INDEX IF EXISTS idx_room_chat_room;
DROP TABLE IF EXISTS room_chat;
DROP TABLE IF EXISTS rooms;

-- +goose StatementEnd
