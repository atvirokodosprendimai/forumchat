-- +goose Up
-- user_reports is a lightweight moderation queue: a member reports
-- another member (optionally referencing a chat message / context) with
-- a free-text reason. Mods/admins see open reports in /admin and resolve
-- them. Nothing is auto-actioned — a report is a signal, not a sanction.
CREATE TABLE user_reports (
    id               TEXT PRIMARY KEY,
    reporter_id      TEXT NOT NULL,
    reported_user_id TEXT NOT NULL,
    community_id     TEXT NOT NULL,
    reason           TEXT NOT NULL,
    context_ref      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'open',  -- open | resolved
    created_at       INTEGER NOT NULL,
    FOREIGN KEY (reporter_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (reported_user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX idx_user_reports_open ON user_reports (community_id, status, created_at);

-- +goose Down
DROP TABLE user_reports;
