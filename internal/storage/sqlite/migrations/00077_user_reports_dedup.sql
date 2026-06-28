-- +goose Up
-- +goose StatementBegin

-- FIX1 N7: chat.PostReport had no per-target cap, so one member could flood the
-- mod queue with hundreds of identical reports against the same target.
-- A partial unique index makes a second OPEN report for the same
-- (reporter, reported_user, community, context_ref) a no-op (the handler now
-- inserts with ON CONFLICT DO NOTHING). Resolved/dismissed reports leave the
-- index, so a fresh report can be filed again after one is closed.

-- Collapse any pre-existing duplicate OPEN reports first, keeping one per group,
-- otherwise the UNIQUE index creation would fail on existing data.
DELETE FROM user_reports
WHERE status = 'open' AND id NOT IN (
    SELECT MIN(id) FROM user_reports
    WHERE status = 'open'
    GROUP BY reporter_id, reported_user_id, community_id, context_ref
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_reports_open_unique
    ON user_reports (reporter_id, reported_user_id, community_id, context_ref)
    WHERE status = 'open';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_reports_open_unique;

-- +goose StatementEnd
