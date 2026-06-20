-- +goose Up
-- +goose StatementBegin

-- Generalise the per-community singleton ai_configs into multiple named
-- agents. Each agent is a full, independent config (its own provider /
-- connection / model / key / system prompt) plus a `vision` flag that, when
-- set, lets members attach an image to that agent's chats (sent to the model;
-- Ollama only accepts images, document/PDF support arrives with the hosted
-- providers). A thread pins to exactly one agent for its lifetime.
CREATE TABLE ai_agents (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT 'Assistant',
    provider      TEXT NOT NULL DEFAULT 'ollama',
    base_url      TEXT NOT NULL DEFAULT 'http://localhost:11434',
    model         TEXT NOT NULL DEFAULT 'llama3.2',
    api_key_enc   TEXT NOT NULL DEFAULT '',
    system_prompt TEXT NOT NULL DEFAULT '',
    vision        INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 0,
    position      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0,
    updated_by    TEXT REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX idx_ai_agents_community ON ai_agents(community_id, position);

-- Carry the existing single config across as a default "Assistant" agent so
-- already-configured communities keep working without a re-setup.
INSERT INTO ai_agents (id, community_id, name, provider, base_url, model, api_key_enc, system_prompt, vision, enabled, position, created_at, updated_at, updated_by)
SELECT lower(hex(randomblob(16))), community_id, 'Assistant', provider, base_url, model, api_key_enc, system_prompt, 0, enabled, 0,
       COALESCE(updated_at, 0), COALESCE(updated_at, 0), updated_by
FROM ai_configs;

-- Pin every existing thread to its community's migrated default agent.
ALTER TABLE ai_threads ADD COLUMN agent_id TEXT REFERENCES ai_agents(id) ON DELETE CASCADE;
UPDATE ai_threads SET agent_id = (
    SELECT a.id FROM ai_agents a WHERE a.community_id = ai_threads.community_id ORDER BY a.position LIMIT 1
);

-- Image attachments for vision agents: JSON array of base64 strings (Ollama's
-- /api/chat `images` shape). Read only at generation time, never in the
-- 100ms fat-morph render path, so it never bloats the stream.
ALTER TABLE ai_messages ADD COLUMN images TEXT NOT NULL DEFAULT '';

DROP TABLE ai_configs;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE ai_configs (
    community_id  TEXT PRIMARY KEY REFERENCES communities(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL DEFAULT 'ollama',
    base_url      TEXT NOT NULL DEFAULT 'http://localhost:11434',
    model         TEXT NOT NULL DEFAULT 'llama3.2',
    api_key_enc   TEXT NOT NULL DEFAULT '',
    system_prompt TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 0,
    updated_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at    INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO ai_configs (community_id, provider, base_url, model, api_key_enc, system_prompt, enabled, updated_by, updated_at)
SELECT community_id, provider, base_url, model, api_key_enc, system_prompt, enabled, updated_by, updated_at
FROM ai_agents WHERE position = 0;
DROP TABLE ai_agents;
-- ai_threads.agent_id and ai_messages.images columns are left in place (SQLite
-- DROP COLUMN is fine but harmless to keep on a down-migration).
-- +goose StatementEnd
