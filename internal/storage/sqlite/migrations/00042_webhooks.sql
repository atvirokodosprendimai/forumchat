-- +goose Up

-- Per-community webhooks in two directions.
--   direction='in'  — external systems POST to /hooks/<token>; the payload is
--                     parsed by a provider adapter and posted as a bot message
--                     into channel_id (NOT NULL for inbound). token is the URL
--                     secret; secret is an optional HMAC signing key (github).
--   direction='out' — new chat messages in channel_id (NULL = all channels) are
--                     relayed as a JSON POST to target_url.
-- channel_id CASCADEs with the channel; the row CASCADEs with the community
-- (super-admin delete blast radius, CLAUDE.md §5d).
CREATE TABLE webhooks (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    direction    TEXT NOT NULL CHECK (direction IN ('in','out')),
    provider     TEXT NOT NULL,
    name         TEXT NOT NULL,
    avatar_url   TEXT NOT NULL DEFAULT '',
    channel_id   TEXT REFERENCES chat_channels(id) ON DELETE CASCADE,
    token        TEXT NOT NULL DEFAULT '',
    secret       TEXT NOT NULL DEFAULT '',
    target_url   TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_by   TEXT REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    last_at      INTEGER,
    last_status  TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_webhooks_token ON webhooks(token) WHERE token <> '';
CREATE INDEX idx_webhooks_community ON webhooks(community_id, direction);

-- Bot identity for kind='webhook' chat messages, denormalised so the chat read
-- path never joins the webhooks table. Empty for every other message kind.
ALTER TABLE chat_messages ADD COLUMN bot_name TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_messages ADD COLUMN bot_avatar_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE chat_messages DROP COLUMN bot_avatar_url;
ALTER TABLE chat_messages DROP COLUMN bot_name;
DROP INDEX IF EXISTS idx_webhooks_community;
DROP INDEX IF EXISTS idx_webhooks_token;
DROP TABLE IF EXISTS webhooks;
