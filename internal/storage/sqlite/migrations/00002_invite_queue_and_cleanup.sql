-- +goose Up
-- +goose StatementBegin

-- Memberships gain an approval gate. NULL = awaiting admin review.
-- Existing rows are retroactively approved at their created_at so MVP users
-- don't suddenly get bounced to a "pending" page after upgrade.
ALTER TABLE memberships ADD COLUMN approved_at INTEGER;
UPDATE memberships SET approved_at = created_at WHERE approved_at IS NULL;

-- Invite codes become "Discord-style" — reusable until max_uses (NULL = unlimited).
-- `used_by` / `used_at` stay populated with the FIRST consumer for legacy
-- single-use lookups, but consumption now increments uses_count and validates
-- against max_uses.
ALTER TABLE invite_codes ADD COLUMN max_uses INTEGER;
ALTER TABLE invite_codes ADD COLUMN uses_count INTEGER NOT NULL DEFAULT 0;
-- For codes already consumed once, mark uses_count=1 so the math stays right.
UPDATE invite_codes SET uses_count = 1 WHERE used_at IS NOT NULL;

-- Chat replies: a message can point at an earlier message in the same channel.
-- NULL = not a reply. We render a small quote-snippet above the reply body.
ALTER TABLE chat_messages ADD COLUMN reply_to_id TEXT REFERENCES chat_messages(id) ON DELETE SET NULL;
CREATE INDEX idx_chat_messages_reply_to ON chat_messages(reply_to_id);

-- Persistent sessions so users don't have to re-login on every restart.
-- Implements the alexedwards/scs/v2 Store interface from a custom file in
-- internal/auth/sqlstore.go.
CREATE TABLE sessions (
    token  TEXT PRIMARY KEY,
    data   BLOB NOT NULL,
    expiry INTEGER NOT NULL
);
CREATE INDEX idx_sessions_expiry ON sessions(expiry);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- SQLite doesn't easily drop columns prior to 3.35; the down migration is
-- best-effort: restore approved_at to NULL semantics has no clean rollback,
-- so we just leave the columns in place.
SELECT 1;
-- +goose StatementEnd
