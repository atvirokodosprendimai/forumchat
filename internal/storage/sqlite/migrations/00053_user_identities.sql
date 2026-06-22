-- +goose Up
-- +goose StatementBegin

-- Linked external (OAuth) identities. One local user can have many provider
-- links (Google, Facebook, …). Resolution: (provider, provider_user_id) is the
-- stable key the callback looks up first; email/name/avatar are cached for
-- account-linking + display and refreshed on every sign-in.
CREATE TABLE user_identities (
    provider          TEXT NOT NULL,
    provider_user_id  TEXT NOT NULL,
    user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email             TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    avatar_url        TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX idx_user_identities_user ON user_identities(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE user_identities;
-- +goose StatementEnd
