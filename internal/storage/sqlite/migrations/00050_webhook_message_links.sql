-- +goose Up
-- Maps an external message id (e.g. a Matrix event id) to the forumchat chat
-- message it was relayed into, per inbound webhook. This is the chat-direction
-- sibling of webhook_thread_links: it lets a later inbound message carrying a
-- reply_to_key (the external id of an earlier message) be posted as an INLINE
-- chat reply under the mapped message, instead of a flat bubble or a separate
-- forum thread. The first inbound message records message_key -> message_id
-- here; a reply looks up reply_to_key to find its parent. Standalone messages
-- (no reply_to_key) stay flat. The reverse direction is owned by the bridge.
CREATE TABLE webhook_message_links (
    webhook_id   TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    external_key TEXT NOT NULL,
    message_id   TEXT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (webhook_id, external_key)
);
CREATE INDEX idx_webhook_message_links_message ON webhook_message_links(message_id);

-- +goose Down
DROP TABLE webhook_message_links;
