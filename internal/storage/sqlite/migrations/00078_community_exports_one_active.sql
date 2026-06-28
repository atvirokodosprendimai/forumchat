-- +goose Up
-- +goose StatementBegin

-- FIX1 N6: enforce "one active export per community" in the schema. The handler
-- guard (COUNT then INSERT) was a TOCTOU — two concurrent requests could both
-- pass the count and both insert. A partial unique index makes the second insert
-- fail; Repo.Request now wraps the check+insert in a transaction and treats the
-- constraint violation as ErrInProgress.

-- Collapse any pre-existing duplicate active exports first (keep one per
-- community) so the unique index can be created on existing data.
DELETE FROM community_exports
WHERE status IN ('pending', 'building') AND id NOT IN (
    SELECT MIN(id) FROM community_exports
    WHERE status IN ('pending', 'building')
    GROUP BY community_id
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_community_exports_one_active
    ON community_exports (community_id)
    WHERE status IN ('pending', 'building');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_community_exports_one_active;

-- +goose StatementEnd
