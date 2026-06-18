-- +goose Up
-- chat_channels splits a community's single realtime chat into multiple
-- all-public named text channels (Slack/Discord style). Every member
-- reads + writes every non-archived channel — there is no membership
-- table; visibility is "all public". Admins/mods curate the set
-- (create / rename / topic / reorder / archive), capped soft at ~10 in
-- the app layer.
--
-- is_default marks the undeletable #general channel that every community
-- has. position drives switcher order. archived_at != NULL hides a
-- channel from the switcher and makes it read-only (history kept).
-- created_by is NULL for the system-seeded #general.
CREATE TABLE chat_channels (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,
    name         TEXT NOT NULL,
    topic        TEXT NOT NULL DEFAULT '',
    position     INTEGER NOT NULL DEFAULT 0,
    is_default   INTEGER NOT NULL DEFAULT 0,
    archived_at  INTEGER,
    created_by   TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE (community_id, slug)
);
CREATE INDEX idx_chat_channels_community ON chat_channels (community_id, position);

-- Seed one #general per existing community (FK order: channels exist
-- before chat_messages / chat_reads reference them, see AGENTS.md §8).
INSERT INTO chat_channels (id, community_id, slug, name, topic, position, is_default, created_by, created_at)
SELECT lower(hex(randomblob(16))), c.id, 'general', 'general', '', 0, 1, NULL, strftime('%s','now')
FROM communities c;

-- chat_messages gain a channel scope. Nullable + FK so ADD COLUMN is
-- legal; backfilled to #general immediately below. App always sets it
-- on new rows.
ALTER TABLE chat_messages ADD COLUMN channel_id TEXT REFERENCES chat_channels(id) ON DELETE CASCADE;
UPDATE chat_messages
SET channel_id = (
    SELECT ch.id FROM chat_channels ch
    WHERE ch.community_id = chat_messages.community_id AND ch.is_default = 1
)
WHERE channel_id IS NULL;
CREATE INDEX idx_chat_messages_channel_created ON chat_messages (channel_id, created_at);

-- Read receipts become per-channel. The PK changes from
-- (user_id, community_id) to (user_id, channel_id), so rebuild the table
-- (SQLite can't ALTER a primary key). community_id is kept for the
-- memberships join in the readers query.
CREATE TABLE chat_reads_new (
    user_id          TEXT NOT NULL,
    community_id     TEXT NOT NULL,
    channel_id       TEXT NOT NULL,
    last_read_at     INTEGER NOT NULL,
    last_read_msg_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, channel_id)
);
INSERT INTO chat_reads_new (user_id, community_id, channel_id, last_read_at, last_read_msg_id)
SELECT r.user_id, r.community_id,
       (SELECT ch.id FROM chat_channels ch
        WHERE ch.community_id = r.community_id AND ch.is_default = 1),
       r.last_read_at, r.last_read_msg_id
FROM chat_reads r;
DROP TABLE chat_reads;
ALTER TABLE chat_reads_new RENAME TO chat_reads;
CREATE INDEX idx_chat_reads_channel_at ON chat_reads (channel_id, last_read_at);

-- +goose Down
-- Collapse back to the community-scoped read model.
CREATE TABLE chat_reads_old (
    user_id          TEXT NOT NULL,
    community_id     TEXT NOT NULL,
    last_read_at     INTEGER NOT NULL,
    last_read_msg_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, community_id)
);
INSERT OR REPLACE INTO chat_reads_old (user_id, community_id, last_read_at, last_read_msg_id)
SELECT user_id, community_id, last_read_at, last_read_msg_id FROM chat_reads;
DROP TABLE chat_reads;
ALTER TABLE chat_reads_old RENAME TO chat_reads;
CREATE INDEX idx_chat_reads_community_at ON chat_reads (community_id, last_read_at);

DROP INDEX IF EXISTS idx_chat_messages_channel_created;
ALTER TABLE chat_messages DROP COLUMN channel_id;

DROP INDEX IF EXISTS idx_chat_channels_community;
DROP TABLE chat_channels;
