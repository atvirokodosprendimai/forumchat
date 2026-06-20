-- +goose Up
-- +goose StatementBegin

-- Per-community AI assistant ("Agent"). One config row per community holds
-- the provider + connection + model. Ollama needs only base_url + model (no
-- key); api_key_enc is reserved for the later Claude/OpenAI providers and is
-- stored encrypted-at-rest when those land. enabled gates the feature per
-- community independently of the AI_ENABLED instance flag.
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

-- A conversation with the agent. visibility = 'private' (only the creator
-- sees it, like ChatGPT history) or 'shared' (every approved member of the
-- community can read AND continue it). user_id is always the creator.
CREATE TABLE ai_threads (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    visibility   TEXT NOT NULL DEFAULT 'private',
    title        TEXT NOT NULL DEFAULT 'New chat',
    model        TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_ai_threads_shared ON ai_threads(community_id, visibility, updated_at);
CREATE INDEX idx_ai_threads_owner ON ai_threads(community_id, user_id, updated_at);

-- One turn in a thread. role = user | assistant | system. The assistant row
-- is created empty with status='generating' and its body_md/body_html are
-- rewritten on every 100ms flush as the model streams; status moves to
-- 'done' on completion, 'interrupted' on stop/restart (partial kept), or
-- 'error'. author_id credits the member who typed a user turn (NULL for
-- assistant/system).
CREATE TABLE ai_messages (
    id         TEXT PRIMARY KEY,
    thread_id  TEXT NOT NULL REFERENCES ai_threads(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    author_id  TEXT REFERENCES users(id) ON DELETE SET NULL,
    body_md    TEXT NOT NULL DEFAULT '',
    body_html  TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'done',
    error      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_ai_messages_thread ON ai_messages(thread_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS ai_messages;
DROP TABLE IF EXISTS ai_threads;
DROP TABLE IF EXISTS ai_configs;
-- +goose StatementEnd
