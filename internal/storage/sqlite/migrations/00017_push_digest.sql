-- +goose Up
-- Digest mode lets a user say "don't ping me every time something
-- happens, batch updates and send at most one notification every N
-- minutes". digest_minutes = 0 means immediate (the original behaviour).
-- digest_last_at is the unix-seconds timestamp of the last digest we
-- sent for this subscription, so the worker can decide when one is due.
ALTER TABLE push_subscriptions ADD COLUMN digest_minutes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE push_subscriptions ADD COLUMN digest_last_at INTEGER NOT NULL DEFAULT 0;

-- Buffer of pending events that have not been pushed yet because the
-- recipient subscription is in digest mode. The digest worker drains
-- rows for each (user, community) when their digest interval elapses.
CREATE TABLE push_pending (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    community_id TEXT NOT NULL,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    url          TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_push_pending_uc        ON push_pending (user_id, community_id);
CREATE INDEX idx_push_pending_created_at ON push_pending (created_at);

-- +goose Down
DROP TABLE push_pending;
ALTER TABLE push_subscriptions DROP COLUMN digest_last_at;
ALTER TABLE push_subscriptions DROP COLUMN digest_minutes;
