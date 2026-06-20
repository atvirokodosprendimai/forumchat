-- +goose Up

-- 1. "Agent supports tools": gate on whether an agent may call MCP tools.
ALTER TABLE ai_agents ADD COLUMN tools_enabled INTEGER NOT NULL DEFAULT 0;

-- 2. Per-assistant-turn tool-call trace (JSON array). Rendered as chips in the
--    chat bubble so members see when/which MCP tool the agent used.
ALTER TABLE ai_messages ADD COLUMN tool_calls TEXT NOT NULL DEFAULT '';

-- 3. Per-community MCP servers. A community admin connects as many as they want;
--    every tools-enabled agent in the community can use the enabled ones. The
--    internal full-text search server is built-in (in-process) and is NOT a row
--    here — these rows are EXTERNAL servers (stdio subprocess or streamable HTTP).
CREATE TABLE ai_mcp_servers (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    transport     TEXT NOT NULL DEFAULT 'stdio',  -- stdio | http
    command       TEXT NOT NULL DEFAULT '',        -- stdio: executable path
    args          TEXT NOT NULL DEFAULT '',        -- stdio: JSON array of args
    url           TEXT NOT NULL DEFAULT '',        -- http: streamable endpoint
    headers       TEXT NOT NULL DEFAULT '',        -- http: JSON object (auth headers)
    env           TEXT NOT NULL DEFAULT '',        -- stdio: JSON object of env vars
    enabled       INTEGER NOT NULL DEFAULT 1,
    position      INTEGER NOT NULL DEFAULT 0,
    updated_by    TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_ai_mcp_servers_community ON ai_mcp_servers(community_id);

-- 4. The "internal MCP": a community-scoped full-text index over chat + forum.
--    A standalone FTS5 table (text is duplicated here, kept in sync by triggers
--    so the index is correct regardless of which Go write path mutates content).
CREATE VIRTUAL TABLE search_fts USING fts5(
    title,
    body,
    kind UNINDEXED,         -- chat | thread | post
    ref_id UNINDEXED,       -- source row id
    community_id UNINDEXED,
    created_at UNINDEXED
);

-- Backfill existing, non-deleted content.
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT '', body_md, 'chat', id, community_id, created_at
    FROM chat_messages WHERE deleted_at IS NULL AND kind = 'user';
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT subject, body_md, 'thread', id, community_id, created_at
    FROM threads WHERE deleted_at IS NULL;
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT '', p.body_md, 'post', p.id, t.community_id, p.created_at
    FROM posts p JOIN threads t ON t.id = p.thread_id WHERE p.deleted_at IS NULL;

-- chat_messages sync (only human 'user' messages; soft-delete sets deleted_at).
-- +goose StatementBegin
CREATE TRIGGER search_fts_chat_ai AFTER INSERT ON chat_messages
WHEN NEW.deleted_at IS NULL AND NEW.kind = 'user' BEGIN
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        VALUES ('', NEW.body_md, 'chat', NEW.id, NEW.community_id, NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_chat_ad AFTER DELETE ON chat_messages BEGIN
    DELETE FROM search_fts WHERE kind = 'chat' AND ref_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_chat_au AFTER UPDATE ON chat_messages BEGIN
    DELETE FROM search_fts WHERE kind = 'chat' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT '', NEW.body_md, 'chat', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.deleted_at IS NULL AND NEW.kind = 'user';
END;
-- +goose StatementEnd

-- threads sync.
-- +goose StatementBegin
CREATE TRIGGER search_fts_thread_ai AFTER INSERT ON threads
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        VALUES (NEW.subject, NEW.body_md, 'thread', NEW.id, NEW.community_id, NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_thread_ad AFTER DELETE ON threads BEGIN
    DELETE FROM search_fts WHERE kind = 'thread' AND ref_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_thread_au AFTER UPDATE ON threads BEGIN
    DELETE FROM search_fts WHERE kind = 'thread' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT NEW.subject, NEW.body_md, 'thread', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.deleted_at IS NULL;
END;
-- +goose StatementEnd

-- posts sync (community_id resolved via parent thread).
-- +goose StatementBegin
CREATE TRIGGER search_fts_post_ai AFTER INSERT ON posts
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT '', NEW.body_md, 'post', NEW.id, t.community_id, NEW.created_at
        FROM threads t WHERE t.id = NEW.thread_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_post_ad AFTER DELETE ON posts BEGIN
    DELETE FROM search_fts WHERE kind = 'post' AND ref_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_post_au AFTER UPDATE ON posts BEGIN
    DELETE FROM search_fts WHERE kind = 'post' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT '', NEW.body_md, 'post', NEW.id, t.community_id, NEW.created_at
        FROM threads t WHERE t.id = NEW.thread_id AND NEW.deleted_at IS NULL;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS search_fts_post_au;
DROP TRIGGER IF EXISTS search_fts_post_ad;
DROP TRIGGER IF EXISTS search_fts_post_ai;
DROP TRIGGER IF EXISTS search_fts_thread_au;
DROP TRIGGER IF EXISTS search_fts_thread_ad;
DROP TRIGGER IF EXISTS search_fts_thread_ai;
DROP TRIGGER IF EXISTS search_fts_chat_au;
DROP TRIGGER IF EXISTS search_fts_chat_ad;
DROP TRIGGER IF EXISTS search_fts_chat_ai;
DROP TABLE IF EXISTS search_fts;
DROP INDEX IF EXISTS idx_ai_mcp_servers_community;
DROP TABLE IF EXISTS ai_mcp_servers;
ALTER TABLE ai_messages DROP COLUMN tool_calls;
ALTER TABLE ai_agents DROP COLUMN tools_enabled;
