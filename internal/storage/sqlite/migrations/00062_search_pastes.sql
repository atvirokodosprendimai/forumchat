-- +goose Up

-- Make pastes searchable. A paste is community-public once POSTED: on save its
-- URL is dropped into a public channel and posted_at is stamped. Drafts
-- (posted_at NULL) are unsent, author-private work-in-progress and must stay out
-- of both community indexes — the gate is posted_at IS NOT NULL, mirroring the
-- AI "shared/done" visibility gate (00038/00039).
--
-- This wires paste into BOTH indexes the same way every other content kind is:
--   * search_fts  (FTS5, synchronous, migration 00038) — triggers keep it live.
--   * embed_outbox (RAG, asynchronous, migration 00039) — triggers enqueue a
--                   marker; the worker re-reads via the rag loader and embeds.
-- The body indexed is the raw paste source (pastes.body), not the rendered HTML.

-- ---------------------------------------------------------------------------
-- FTS5 (search_fts) — synchronous full-text index.
-- ---------------------------------------------------------------------------

-- Backfill existing posted pastes.
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT title, body, 'paste', id, community_id, created_at
    FROM pastes WHERE posted_at IS NOT NULL;

-- +goose StatementBegin
CREATE TRIGGER search_fts_paste_ai AFTER INSERT ON pastes
WHEN NEW.posted_at IS NOT NULL BEGIN
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        VALUES (NEW.title, NEW.body, 'paste', NEW.id, NEW.community_id, NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_paste_ad AFTER DELETE ON pastes BEGIN
    DELETE FROM search_fts WHERE kind = 'paste' AND ref_id = OLD.id;
END;
-- +goose StatementEnd
-- The draft -> posted transition is an UPDATE (Save stamps posted_at); re-index
-- so a paste enters search_fts the moment it goes public.
-- +goose StatementBegin
CREATE TRIGGER search_fts_paste_au AFTER UPDATE ON pastes BEGIN
    DELETE FROM search_fts WHERE kind = 'paste' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT NEW.title, NEW.body, 'paste', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.posted_at IS NOT NULL;
END;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- RAG (embed_outbox) — asynchronous semantic index. Triggers only enqueue; the
-- worker re-reads via the loader (which re-applies the posted_at gate) so an
-- upsert for a still-draft paste resolves to a no-op delete.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE TRIGGER rag_outbox_paste_ai AFTER INSERT ON pastes
WHEN NEW.posted_at IS NOT NULL BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('paste', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_paste_au AFTER UPDATE ON pastes BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('paste', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_paste_ad AFTER DELETE ON pastes BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('paste', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- Backfill: enqueue existing posted pastes so the first worker run indexes them.
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'paste', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM pastes WHERE posted_at IS NOT NULL;

-- +goose Down
DROP TRIGGER IF EXISTS rag_outbox_paste_ad;
DROP TRIGGER IF EXISTS rag_outbox_paste_au;
DROP TRIGGER IF EXISTS rag_outbox_paste_ai;
DROP TRIGGER IF EXISTS search_fts_paste_au;
DROP TRIGGER IF EXISTS search_fts_paste_ad;
DROP TRIGGER IF EXISTS search_fts_paste_ai;
DELETE FROM embed_outbox WHERE kind = 'paste';
DELETE FROM search_fts WHERE kind = 'paste';
