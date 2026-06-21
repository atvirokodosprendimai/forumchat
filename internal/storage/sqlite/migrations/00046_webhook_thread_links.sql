-- +goose Up
-- Maps an external thread root (e.g. a Matrix thread-root event id) to a
-- forumchat forum thread, per inbound webhook. This is the inbound-direction
-- map that forumchat owns: the first inbound message carrying a given
-- external_key opens a forum thread and records the link here; later messages
-- with the same key append posts to the mapped thread. The reverse direction
-- (forumchat thread -> external root) is owned by the external bridge/plugin.
CREATE TABLE webhook_thread_links (
    webhook_id   TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    external_key TEXT NOT NULL,
    thread_id    TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (webhook_id, external_key)
);
CREATE INDEX idx_webhook_thread_links_thread ON webhook_thread_links(thread_id);

-- +goose Down
DROP TABLE webhook_thread_links;
