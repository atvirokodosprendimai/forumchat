-- +goose Up
-- +goose StatementBegin

-- Per-community tenant configuration for SaaS mode. One row per community, all
-- columns nullable: an unset column means "fall back to the platform env
-- default" (see internal/community/resolve.go). In self-hosted mode (SAAS=false)
-- this table is ignored entirely — the resolver short-circuits to env. Secrets
-- (*_enc) are sealed with internal/secretbox before they are written here.
CREATE TABLE community_settings (
    community_id              TEXT PRIMARY KEY REFERENCES communities(id) ON DELETE CASCADE,

    -- AI master switch (agents themselves live in ai_agents, already per-community)
    ai_enabled                INTEGER,

    -- RAG: per-community embedder + dedicated Qdrant collection (dynamic dim)
    rag_enabled               INTEGER,
    rag_embed_base_url        TEXT,
    rag_embed_model           TEXT,
    rag_embed_dim             INTEGER,
    rag_qdrant_url            TEXT,
    rag_qdrant_api_key_enc    TEXT,
    rag_qdrant_collection     TEXT,

    -- Translation: per-community model + Ollama host
    translate_enabled         INTEGER,
    translate_base_url        TEXT,
    translate_model           TEXT,

    -- Storage: shared platform store (namespaced) or the community's own S3
    storage_backend           TEXT,          -- NULL | 'disk' | 's3'
    storage_s3_endpoint       TEXT,
    storage_s3_region         TEXT,
    storage_s3_bucket         TEXT,
    storage_s3_access_key_enc TEXT,
    storage_s3_secret_key_enc TEXT,
    storage_migrated_at       INTEGER,

    -- Join policy: 'open' (auto-approve) | 'request' (approval queue)
    join_policy               TEXT,

    updated_at                INTEGER
);

-- Records which blob store an upload's bytes live in, so reads resolve the right
-- backend after a community migrates to its own bucket. '' = the legacy/global
-- default store (disk dir or platform S3).
ALTER TABLE uploads ADD COLUMN store_key TEXT NOT NULL DEFAULT '';

-- Promote the earliest-created admin of each community to the new 'owner' role
-- (the community super-admin). Deterministic: one owner per community, chosen by
-- (created_at, id). Communities with no admin are left owner-less; seed via CLI.
UPDATE memberships SET role = 'owner'
WHERE id IN (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY community_id ORDER BY created_at ASC, id ASC
               ) AS rn
        FROM memberships
        WHERE role = 'admin'
    ) WHERE rn = 1
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

UPDATE memberships SET role = 'admin' WHERE role = 'owner';
ALTER TABLE uploads DROP COLUMN store_key;
DROP TABLE community_settings;

-- +goose StatementEnd
