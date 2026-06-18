-- +goose Up
-- user_blocks is a per-viewer mute relation: when blocker_id blocks
-- blocked_id in a community, the blocker stops seeing the blocked user's
-- chat messages (filtered at read time in chat.loadRecentFor — purely a
-- read-model concern, nothing is deleted). Scoped per community so a
-- block in one space doesn't leak to another.
CREATE TABLE user_blocks (
    blocker_id   TEXT NOT NULL,
    blocked_id   TEXT NOT NULL,
    community_id TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (blocker_id, blocked_id, community_id),
    FOREIGN KEY (blocker_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (blocked_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX idx_user_blocks_blocker ON user_blocks (blocker_id, community_id);

-- +goose Down
DROP TABLE user_blocks;
