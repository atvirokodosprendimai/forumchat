-- +goose Up
-- A paste is a long code/markdown/text snippet a member writes on a dedicated
-- page instead of flooding a channel. Created as a draft (posted_at NULL) by the
-- /paste slash command (or the composer button), filled in on the paste page,
-- then posted: on save its URL is dropped into the source channel and posted_at
-- is stamped. channel_id is the source channel for the post-back + return
-- redirect; ON DELETE SET NULL keeps the paste alive if that channel is removed.
CREATE TABLE pastes (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    channel_id   TEXT REFERENCES chat_channels(id) ON DELETE SET NULL,
    author_id    TEXT NOT NULL REFERENCES users(id),
    title        TEXT NOT NULL DEFAULT '',
    language     TEXT NOT NULL DEFAULT 'go',
    body         TEXT NOT NULL DEFAULT '',
    body_html    TEXT NOT NULL DEFAULT '',
    posted_at    INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_pastes_community ON pastes(community_id);

-- +goose Down
DROP TABLE pastes;
