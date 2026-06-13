-- +goose Up
-- +goose StatementBegin

ALTER TABLE threads ADD COLUMN resolved_at INTEGER;
ALTER TABLE threads ADD COLUMN resolved_by TEXT REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX idx_threads_community_resolved ON threads(community_id, resolved_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_threads_community_resolved;
ALTER TABLE threads DROP COLUMN resolved_by;
ALTER TABLE threads DROP COLUMN resolved_at;

-- +goose StatementEnd
