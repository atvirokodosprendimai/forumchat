-- +goose Up

-- When a video room empties (everyone disconnects / the janitor evicts the
-- last stale member) the room resets to its seeded default state: private,
-- no admin, invites revoked, chat archived. "Archived" means the live
-- room_chat rows are MOVED here, so the next session starts with a blank
-- chat while the prior conversation is retained for the record.
--
-- room_id is a plain column (no FK) so the archive survives independently of
-- the live room slot. community_id keeps its CASCADE so a platform-level
-- community delete (super-admin) still cleans the archive up with everything
-- else (see CLAUDE.md §5d blast radius).
CREATE TABLE room_chat_archive (
    id              TEXT PRIMARY KEY,
    room_id         TEXT NOT NULL,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    author_user_id  TEXT,
    author_name     TEXT NOT NULL,
    body            TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    archived_at     INTEGER NOT NULL
);
CREATE INDEX idx_room_chat_archive_room ON room_chat_archive(room_id, archived_at);
CREATE INDEX idx_room_chat_archive_community ON room_chat_archive(community_id, archived_at);

-- +goose Down
DROP INDEX IF EXISTS idx_room_chat_archive_community;
DROP INDEX IF EXISTS idx_room_chat_archive_room;
DROP TABLE IF EXISTS room_chat_archive;
