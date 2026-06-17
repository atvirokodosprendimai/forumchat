-- +goose Up
-- chat_reads tracks the high-water mark each member has read in a
-- community's chat channel. One row per (user, community). last_read_at
-- is unix seconds of the newest chat_message.created_at the user has
-- acknowledged seeing; last_read_msg_id is the message id at that
-- moment (kept for diagnostics / future per-message seen).
--
-- The mark-read endpoint upserts this row whenever the client says
-- "I'm focused and viewing the latest". Read receipts attached to a
-- message M are: rows where last_read_at >= M.created_at AND user_id
-- != M.author_id.
CREATE TABLE chat_reads (
    user_id          TEXT NOT NULL,
    community_id     TEXT NOT NULL,
    last_read_at     INTEGER NOT NULL,
    last_read_msg_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, community_id)
);
CREATE INDEX idx_chat_reads_community_at
    ON chat_reads (community_id, last_read_at);

-- +goose Down
DROP TABLE chat_reads;
