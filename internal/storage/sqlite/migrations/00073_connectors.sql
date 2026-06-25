-- +goose Up

-- Per-community "external chat bot" connectors. A connector lets an arbitrary
-- external worker participate in chat AS A HUMAN: it holds open a signed SSE
-- stream (/bots/<id>/stream) to receive realtime messages and POSTs back
-- (/bots/<id>/send, body-HMAC signed). Each connector is backed by a real
-- synthetic member (user_id) so its sends are ordinary kind='user' messages —
-- roster, @mention, profile and mod-delete all work with no special-casing.
--
-- secret is the per-connector HMAC key (crypto/rand): it both signs the stream
-- URL and verifies the X-Signature on /send. Rotating it revokes both at once.
-- The row CASCADEs with the community (super-admin delete blast radius, §5d);
-- user_id has NO cascade so deleting the connector is an explicit member-removal
-- in the service layer, not a silent FK side effect.
--
-- capabilities is a CSV set of the moderation powers the community admin grants
-- this connector (e.g. 'send,delete,ban,rename'). 'send' is the base ability to
-- post; the rest enable matching signed action endpoints (/bots/<id>/delete etc).
-- A CSV set (not boolean columns) keeps the grant list open-ended without a
-- migration per new capability. Default 'send' = post-only, no moderation.
CREATE TABLE connectors (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users(id),
    name          TEXT NOT NULL,
    avatar_url    TEXT NOT NULL DEFAULT '',
    secret        TEXT NOT NULL,
    capabilities  TEXT NOT NULL DEFAULT 'send',
    mentions_only INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_by    TEXT REFERENCES users(id),
    created_at    INTEGER NOT NULL,
    last_seen_at  INTEGER,
    last_status   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_connectors_community ON connectors(community_id);

-- The channel allowlist a connector joins. An EMPTY set (no rows) means "all
-- non-archived channels" — resolved in the query layer, not stored as a wildcard
-- row. Both sides CASCADE: dropping a channel or a connector prunes its links.
CREATE TABLE connector_channels (
    connector_id TEXT NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
    channel_id   TEXT NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (connector_id, channel_id)
);

-- +goose Down
DROP TABLE IF EXISTS connector_channels;
DROP INDEX IF EXISTS idx_connectors_community;
DROP TABLE IF EXISTS connectors;
