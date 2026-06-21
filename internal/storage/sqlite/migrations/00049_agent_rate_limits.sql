-- +goose Up
-- +goose StatementBegin

-- Per-community AI-agent prompt rate limits (requests/minute). 0 = unlimited.
-- agent_rate_per_user_min     caps each member's triggers, community-wide.
-- agent_rate_per_community_min caps all members combined, community-wide.
-- Superadmins bypass both at runtime; community admins are subject to them.
ALTER TABLE communities ADD COLUMN agent_rate_per_user_min INTEGER NOT NULL DEFAULT 5;
ALTER TABLE communities ADD COLUMN agent_rate_per_community_min INTEGER NOT NULL DEFAULT 60;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE communities DROP COLUMN agent_rate_per_user_min;
ALTER TABLE communities DROP COLUMN agent_rate_per_community_min;

-- +goose StatementEnd
