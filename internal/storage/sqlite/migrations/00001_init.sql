-- +goose Up
-- +goose StatementBegin

CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE verification_tokens (
    token       TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose     TEXT NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER
);
CREATE INDEX idx_verification_tokens_user ON verification_tokens(user_id);

CREATE TABLE communities (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE memberships (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    display_name    TEXT NOT NULL,
    avatar_url      TEXT NOT NULL DEFAULT '',
    role            TEXT NOT NULL DEFAULT 'member',
    trust_level     INTEGER NOT NULL DEFAULT 0,
    banned_until    INTEGER,
    created_at      INTEGER NOT NULL,
    UNIQUE (user_id, community_id)
);
CREATE INDEX idx_memberships_community ON memberships(community_id);

CREATE TABLE invite_codes (
    code            TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    created_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
    used_by         TEXT REFERENCES users(id) ON DELETE SET NULL,
    used_at         INTEGER,
    expires_at      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_invite_codes_community ON invite_codes(community_id);

CREATE TABLE chat_messages (
    id              TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    author_id       TEXT REFERENCES users(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL DEFAULT 'user',
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    ref_thread_id   TEXT,
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_chat_messages_community_created ON chat_messages(community_id, created_at);

CREATE TABLE threads (
    id              TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    author_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject         TEXT NOT NULL,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    deleted_at      INTEGER,
    last_activity_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_threads_community_activity ON threads(community_id, last_activity_at);

CREATE TABLE posts (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    author_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    quoted_post_id  TEXT REFERENCES posts(id) ON DELETE SET NULL,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_posts_thread_created ON posts(thread_id, created_at);

CREATE TABLE uploads (
    id              TEXT PRIMARY KEY,
    owner_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    sha256          TEXT NOT NULL,
    mime            TEXT NOT NULL,
    size            INTEGER NOT NULL,
    rel_path        TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_uploads_owner ON uploads(owner_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS uploads;
DROP TABLE IF EXISTS posts;
DROP TABLE IF EXISTS threads;
DROP TABLE IF EXISTS chat_messages;
DROP TABLE IF EXISTS invite_codes;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS communities;
DROP TABLE IF EXISTS verification_tokens;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
