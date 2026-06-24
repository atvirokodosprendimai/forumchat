-- +goose Up
-- +goose StatementBegin

-- ai_usage_events is the append-only metering ledger for PLATFORM-PROVIDED AI
-- compute (eidos/spec - saas-platform-ai …). When a community opts into the
-- platform's hosted AI ("use system-wide settings") and is authorized
-- (super-admin grant OR active Stripe subscription), every RAG-embed /
-- translate / agent request it makes on the operator's compute writes one row
-- here. BYO requests write nothing — so the ledger equals the operator's cost
-- equals the bill. Token counts are real for agent generation (Ollama
-- prompt_eval_count / eval_count) and estimated (estimated=1) for embed /
-- translate, which return no usage.
--
-- The table is append-only: aggregates (per community/feature/day, totals) are
-- computed by query, never mutated in place. community_id cascades on delete;
-- user_id is a soft stamp (SET NULL) so a metered request survives the member
-- being erased.
CREATE TABLE ai_usage_events (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    feature       TEXT NOT NULL,                 -- agent | rag_embed | translate
    user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,  -- NULL = background work
    model         TEXT NOT NULL DEFAULT '',
    tokens_in     INTEGER NOT NULL DEFAULT 0,
    tokens_out    INTEGER NOT NULL DEFAULT 0,
    estimated     INTEGER NOT NULL DEFAULT 0,    -- 1 = token counts are estimated, not provider-reported
    created_at    INTEGER NOT NULL
);

-- Drives the per-community date-range rollups (owner + super-admin usage panels).
CREATE INDEX idx_ai_usage_events_community ON ai_usage_events(community_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_ai_usage_events_community;
DROP TABLE IF EXISTS ai_usage_events;

-- +goose StatementEnd
