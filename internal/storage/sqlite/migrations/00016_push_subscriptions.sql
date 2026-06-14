-- +goose Up
-- One row per (user, community, endpoint). Endpoint is the push service
-- URL the browser hands back from PushSubscription; it is the natural
-- key for unsubscribe / replace flows. settings_json is a small JSON
-- object the client owns shape of, e.g.
--   {"mention":true,"report":true,"project_new":false,"comment_new":true}
CREATE TABLE push_subscriptions (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    community_id  TEXT NOT NULL,
    endpoint      TEXT NOT NULL,
    p256dh        TEXT NOT NULL,
    auth_key      TEXT NOT NULL,
    user_agent    TEXT NOT NULL DEFAULT '',
    settings_json TEXT NOT NULL DEFAULT '{}',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (user_id, community_id, endpoint)
);
CREATE INDEX idx_push_subscriptions_user_community
    ON push_subscriptions (user_id, community_id);
CREATE INDEX idx_push_subscriptions_endpoint
    ON push_subscriptions (endpoint);

-- +goose Down
DROP TABLE push_subscriptions;
