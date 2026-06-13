-- +goose Up
-- +goose StatementBegin

-- users.status is TEXT without CHECK; the new 'invited' value is accepted
-- as-is. UserStatus constants gain the new value in Go code.

CREATE TABLE signup_tokens (
    token         TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    expires_at    INTEGER NOT NULL,
    used_at       INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_signup_tokens_user ON signup_tokens(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_signup_tokens_user;
DROP TABLE IF EXISTS signup_tokens;

-- +goose StatementEnd
