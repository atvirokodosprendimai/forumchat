-- +goose Up
-- +goose StatementBegin

-- community_requests backs the SaaS self-serve flow: a registered user may
-- create their FIRST community instantly (they become its owner), but any
-- additional community must be approved by a platform super-admin. An
-- over-quota user files a pending request here; the super-admin approves (which
-- provisions the community with the requester as owner and stamps community_id)
-- or denies it.
--
-- No FK on community_id / decided_by: they are informational stamps that must
-- survive even if the created community is later deleted or the deciding
-- super-admin's account is removed. user_id cascades — a deleted user's pending
-- requests are meaningless.
CREATE TABLE community_requests (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',  -- 'pending' | 'approved' | 'denied'
    community_id TEXT,                             -- set on approve: the provisioned community
    decided_by   TEXT,                             -- super-admin user id who decided
    created_at   INTEGER NOT NULL,
    decided_at   INTEGER
);

CREATE INDEX idx_community_requests_status ON community_requests(status, created_at);
CREATE INDEX idx_community_requests_user ON community_requests(user_id, status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_community_requests_user;
DROP INDEX IF EXISTS idx_community_requests_status;
DROP TABLE IF EXISTS community_requests;

-- +goose StatementEnd
