-- +goose Up

-- RAG vector indexing — the async sibling of search_fts (00038).
--
-- Vectors CANNOT be maintained inline by triggers the way FTS5 is: producing an
-- embedding requires an HTTP call to Ollama, which is illegal inside a SQLite
-- trigger. So instead of writing the index directly, the triggers below enqueue
-- a tiny "this row changed" marker into embed_outbox; a background worker
-- (internal/rag.Worker) drains the queue, embeds, and upserts into the vector
-- store. This is the same trigger-as-source-of-truth philosophy as search_fts,
-- only the index update is deferred and backend-agnostic (chromem-go now,
-- qdrant later) — see internal/rag.
--
-- The outbox carries no community_id: an upsert re-reads the row (the loader
-- resolves the community + applies visibility gating), and a delete only needs
-- (kind, ref_id) because the vector store is a single collection filtered by a
-- community_id metadata field. UNIQUE(kind, ref_id) coalesces repeated edits to
-- the same row into one pending job (latest op wins).
CREATE TABLE embed_outbox (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,   -- chat|thread|post|issue|issue_comment|discussion|discussion_reply|ai|project
    ref_id      TEXT NOT NULL,   -- source row id
    op          TEXT NOT NULL,   -- upsert | delete
    enqueued_at INTEGER NOT NULL,
    UNIQUE(kind, ref_id)
);

-- The triggers below are intentionally cheap and always-on (like search_fts's).
-- When RAG is disabled the worker never drains, so the outbox holds at most one
-- row per content item (UNIQUE coalescing) — bounded, not a leak. Enabling RAG
-- later drains the backlog and indexes history.

-- +goose StatementBegin
CREATE TRIGGER rag_outbox_chat_ai AFTER INSERT ON chat_messages
WHEN NEW.kind = 'user' AND NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('chat', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_chat_au AFTER UPDATE ON chat_messages BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('chat', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_chat_ad AFTER DELETE ON chat_messages BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('chat', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- threads
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_thread_ai AFTER INSERT ON threads
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('thread', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_thread_au AFTER UPDATE ON threads BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('thread', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_thread_ad AFTER DELETE ON threads BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('thread', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- posts
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_post_ai AFTER INSERT ON posts
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('post', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_post_au AFTER UPDATE ON posts BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('post', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_post_ad AFTER DELETE ON posts BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('post', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- project_issues (no deleted_at — hard CASCADE delete)
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_issue_ai AFTER INSERT ON project_issues BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_issue_au AFTER UPDATE ON project_issues BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_issue_ad AFTER DELETE ON project_issues BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- project_issue_comments
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_icomment_ai AFTER INSERT ON project_issue_comments
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue_comment', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_icomment_au AFTER UPDATE ON project_issue_comments BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue_comment', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_icomment_ad AFTER DELETE ON project_issue_comments BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('issue_comment', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- project_discussion_threads
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_disc_ai AFTER INSERT ON project_discussion_threads
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_disc_au AFTER UPDATE ON project_discussion_threads BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_disc_ad AFTER DELETE ON project_discussion_threads BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- project_discussion_replies
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_dreply_ai AFTER INSERT ON project_discussion_replies
WHEN NEW.deleted_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion_reply', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_dreply_au AFTER UPDATE ON project_discussion_replies BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion_reply', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_dreply_ad AFTER DELETE ON project_discussion_replies BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('discussion_reply', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- projects (description)
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_project_ai AFTER INSERT ON projects
WHEN NEW.archived_at IS NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('project', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_project_au AFTER UPDATE ON projects BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('project', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_project_ad AFTER DELETE ON projects BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('project', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- ai_messages — only assistant turns. Visibility/status gating is applied by the
-- worker's loader at process time (an upsert for a not-yet-done or private-thread
-- message resolves to a no-op delete; the row gets re-enqueued when it completes
-- or its thread is shared, see the ai_threads trigger below).
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_ai_ai AFTER INSERT ON ai_messages
WHEN NEW.role = 'assistant' BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('ai', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_ai_au AFTER UPDATE ON ai_messages
WHEN NEW.role = 'assistant' BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('ai', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_ai_ad AFTER DELETE ON ai_messages BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('ai', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- A thread flipping private<->shared changes whether ALL its assistant messages
-- are community-visible, so re-enqueue them on visibility change.
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_aithread_visibility AFTER UPDATE OF visibility ON ai_threads BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'ai', m.id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM ai_messages m WHERE m.thread_id = NEW.id AND m.role = 'assistant'
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- Backfill: enqueue all existing community-public content so the first worker
-- run (once RAG is enabled) indexes history. INSERT OR IGNORE because triggers
-- above may already have fired during this migration's table reads (they don't,
-- but the guard is free and keeps re-runs idempotent).
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'chat', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM chat_messages WHERE kind='user' AND deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'thread', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM threads WHERE deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'post', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM posts WHERE deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'issue', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM project_issues;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'issue_comment', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM project_issue_comments WHERE deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'discussion', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM project_discussion_threads WHERE deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'discussion_reply', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM project_discussion_replies WHERE deleted_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'project', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM projects WHERE archived_at IS NULL;
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'ai', m.id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM ai_messages m JOIN ai_threads t ON t.id = m.thread_id
    WHERE m.role='assistant' AND m.status='done' AND t.visibility='shared';

-- +goose Down
DROP TRIGGER IF EXISTS rag_outbox_aithread_visibility;
DROP TRIGGER IF EXISTS rag_outbox_ai_ad;
DROP TRIGGER IF EXISTS rag_outbox_ai_au;
DROP TRIGGER IF EXISTS rag_outbox_ai_ai;
DROP TRIGGER IF EXISTS rag_outbox_project_ad;
DROP TRIGGER IF EXISTS rag_outbox_project_au;
DROP TRIGGER IF EXISTS rag_outbox_project_ai;
DROP TRIGGER IF EXISTS rag_outbox_dreply_ad;
DROP TRIGGER IF EXISTS rag_outbox_dreply_au;
DROP TRIGGER IF EXISTS rag_outbox_dreply_ai;
DROP TRIGGER IF EXISTS rag_outbox_disc_ad;
DROP TRIGGER IF EXISTS rag_outbox_disc_au;
DROP TRIGGER IF EXISTS rag_outbox_disc_ai;
DROP TRIGGER IF EXISTS rag_outbox_icomment_ad;
DROP TRIGGER IF EXISTS rag_outbox_icomment_au;
DROP TRIGGER IF EXISTS rag_outbox_icomment_ai;
DROP TRIGGER IF EXISTS rag_outbox_issue_ad;
DROP TRIGGER IF EXISTS rag_outbox_issue_au;
DROP TRIGGER IF EXISTS rag_outbox_issue_ai;
DROP TRIGGER IF EXISTS rag_outbox_post_ad;
DROP TRIGGER IF EXISTS rag_outbox_post_au;
DROP TRIGGER IF EXISTS rag_outbox_post_ai;
DROP TRIGGER IF EXISTS rag_outbox_thread_ad;
DROP TRIGGER IF EXISTS rag_outbox_thread_au;
DROP TRIGGER IF EXISTS rag_outbox_thread_ai;
DROP TRIGGER IF EXISTS rag_outbox_chat_ad;
DROP TRIGGER IF EXISTS rag_outbox_chat_au;
DROP TRIGGER IF EXISTS rag_outbox_chat_ai;
DROP TABLE IF EXISTS embed_outbox;
