-- +goose Up

-- Lobbies — tokenized guest access to a community without requiring an
-- account. Each row binds one guest (no multi-party in v1) to one host
-- via a random URL-safe token. `medium` distinguishes the text-chat
-- variant from the future video-room variant; v1 only mints `lobby`
-- rows. Status drives URL behaviour: open serves chat, archived hides
-- from the default host list but keeps the URL working, closed returns
-- 410 to the guest while keeping history visible to the host.
CREATE TABLE lobbies (
    id                  TEXT PRIMARY KEY,
    community_id        TEXT NOT NULL REFERENCES communities(id),
    host_user_id        TEXT NOT NULL REFERENCES users(id),
    medium              TEXT NOT NULL DEFAULT 'lobby',
    guest_display_name  TEXT NOT NULL DEFAULT '',
    guest_email         TEXT NOT NULL DEFAULT '',
    guest_token         TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL DEFAULT 'open',
    expires_at          INTEGER,
    created_at          INTEGER NOT NULL,
    last_activity_at    INTEGER NOT NULL
);
CREATE INDEX idx_lobbies_community_status ON lobbies(community_id, status, last_activity_at DESC);
CREATE INDEX idx_lobbies_host             ON lobbies(host_user_id, status, last_activity_at DESC);

-- Persistent message history. author_kind distinguishes host vs guest
-- (the guest has no user_id, so author_user_id is NULL for them).
-- Soft-delete via deleted_at matches chat_messages so the same render
-- pipeline can be reused.
CREATE TABLE lobby_messages (
    id              TEXT PRIMARY KEY,
    lobby_id        TEXT NOT NULL REFERENCES lobbies(id) ON DELETE CASCADE,
    author_kind     TEXT NOT NULL,
    author_user_id  TEXT,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    deleted_at      INTEGER
);
CREATE INDEX idx_lobby_messages_lobby_created ON lobby_messages(lobby_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_lobby_messages_lobby_created;
DROP TABLE IF EXISTS lobby_messages;
DROP INDEX IF EXISTS idx_lobbies_host;
DROP INDEX IF EXISTS idx_lobbies_community_status;
DROP TABLE IF EXISTS lobbies;
