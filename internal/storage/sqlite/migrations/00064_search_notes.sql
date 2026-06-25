-- +goose Up
-- Make notes searchable. A note is community-visible only when PUBLIC; a private
-- note is unlisted (readable solely via its share-link) and must stay out of both
-- community indexes — the gate is visibility = 'public', mirroring the paste
-- posted_at gate (00062) and the AI shared/done gate (00038/00039).
--
-- Wires note into BOTH indexes the same way every other content kind is:
--   * search_fts   (FTS5, synchronous, 00038) — triggers keep it live.
--   * embed_outbox (RAG, asynchronous, 00039) — triggers enqueue a marker; the
--                  worker re-reads via the rag loader (which re-applies the
--                  visibility gate) and embeds. Indexed body is notes.body (the
--                  raw markdown source), not the rendered HTML.

-- ---------------------------------------------------------------------------
-- FTS5 (search_fts) — synchronous full-text index.
-- ---------------------------------------------------------------------------

-- Backfill existing public notes.
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT title, body, 'note', id, community_id, created_at
    FROM notes WHERE visibility = 'public';

-- +goose StatementBegin
CREATE TRIGGER search_fts_note_ai AFTER INSERT ON notes
WHEN NEW.visibility = 'public' BEGIN
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        VALUES (NEW.title, NEW.body, 'note', NEW.id, NEW.community_id, NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_note_ad AFTER DELETE ON notes BEGIN
    DELETE FROM search_fts WHERE kind = 'note' AND ref_id = OLD.id;
END;
-- +goose StatementEnd
-- A note's body and its private<->public visibility both change on UPDATE; drop
-- then re-insert iff it is currently public, so the index always reflects the
-- live row (a note going private leaves the index, a note going public enters).
-- +goose StatementBegin
CREATE TRIGGER search_fts_note_au AFTER UPDATE ON notes BEGIN
    DELETE FROM search_fts WHERE kind = 'note' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT NEW.title, NEW.body, 'note', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.visibility = 'public';
END;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- RAG (embed_outbox) — asynchronous semantic index. Triggers only enqueue; the
-- worker re-reads via the loader (which re-applies the visibility gate) so an
-- upsert for a now-private note resolves to a no-op delete.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE TRIGGER rag_outbox_note_ai AFTER INSERT ON notes
WHEN NEW.visibility = 'public' BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('note', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_note_au AFTER UPDATE ON notes BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('note', NEW.id, 'upsert', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER rag_outbox_note_ad AFTER DELETE ON notes BEGIN
    INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
    VALUES ('note', OLD.id, 'delete', CAST(strftime('%s','now') AS INTEGER))
    ON CONFLICT(kind, ref_id) DO UPDATE SET op='delete', enqueued_at=excluded.enqueued_at;
END;
-- +goose StatementEnd

-- Backfill: enqueue existing public notes so the first worker run indexes them.
INSERT OR IGNORE INTO embed_outbox(kind, ref_id, op, enqueued_at)
    SELECT 'note', id, 'upsert', CAST(strftime('%s','now') AS INTEGER)
    FROM notes WHERE visibility = 'public';

-- +goose Down
DROP TRIGGER IF EXISTS rag_outbox_note_ad;
DROP TRIGGER IF EXISTS rag_outbox_note_au;
DROP TRIGGER IF EXISTS rag_outbox_note_ai;
DROP TRIGGER IF EXISTS search_fts_note_au;
DROP TRIGGER IF EXISTS search_fts_note_ad;
DROP TRIGGER IF EXISTS search_fts_note_ai;
DELETE FROM embed_outbox WHERE kind = 'note';
DELETE FROM search_fts WHERE kind = 'note';
