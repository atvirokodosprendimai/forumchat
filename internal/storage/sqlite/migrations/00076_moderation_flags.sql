-- +goose Up
-- +goose StatementBegin

-- moderation_flags is the privacy-preserving audit trail of the automated
-- safety classifier (Phase B, internal/moderation). When MODERATION_MODEL is
-- configured, every user chat message is classified by a Llama Guard model on
-- Ollama; a policy hit writes ONE row here.
--
-- Deliberately stores NO message body — only a reference (message_id) and the
-- policy CATEGORY codes (e.g. "S3,S12"). This is what lets abuse surface to the
-- platform super-admin as counts + categories WITHOUT breaching the SaaS
-- privacy wall (the operator cannot read tenant content — see
-- auth.Identity.GodMode). The Red flags panel reads aggregates from this table
-- (flagged count + distinct flagged authors over 24h).
--
-- community_id cascades on delete (a deleted tenant's audit goes with it).
-- author_id is a soft stamp (SET NULL) so a flag survives the member being
-- erased; it still feeds the "one actor vs many" author-spread signal while
-- present.
CREATE TABLE moderation_flags (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    message_id    TEXT NOT NULL,
    channel_id    TEXT,
    author_id     TEXT REFERENCES users(id) ON DELETE SET NULL,
    categories    TEXT NOT NULL DEFAULT '',  -- CSV of Llama Guard S-codes; never message content
    model         TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

-- Drives the 24h red-flag aggregates (count + distinct authors) per community.
CREATE INDEX idx_moderation_flags_community ON moderation_flags(community_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_moderation_flags_community;
DROP TABLE IF EXISTS moderation_flags;

-- +goose StatementEnd
