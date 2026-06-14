-- +goose Up
-- +goose StatementBegin

ALTER TABLE communities ADD COLUMN is_public INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_communities_is_public ON communities(is_public);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_communities_is_public;
ALTER TABLE communities DROP COLUMN is_public;

-- +goose StatementEnd
