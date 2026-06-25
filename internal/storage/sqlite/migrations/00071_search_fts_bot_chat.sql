-- +goose Up

-- Make finished agent (kind='bot') chat replies searchable in the FTS index,
-- matching the RAG vector loader's rule (internal/rag/repo.go KindChat). Until
-- now both indexes skipped bot bubbles: a triggered agent's in-channel answer
-- (§6.9.0) is public channel content like any other message, yet it never
-- surfaced in search.
--
-- The original chat AU trigger (migration 00038) re-inserts the FTS row only
-- WHEN kind='user', so a bot bubble — inserted empty then streamed via
-- UpdateBotBody — was always filtered out. We replace that trigger so the
-- re-insert also fires for a COMPLETED bot bubble (gen_status='done'); a
-- 'generating'/'interrupted' row holds a partial answer and stays out until the
-- final done-UPDATE re-fires this trigger. The INSERT trigger
-- (search_fts_chat_ai) is left untouched: bots are always inserted as an empty
-- 'generating' placeholder, so the done transition only ever arrives as an
-- UPDATE — the same reason the RAG INSERT trigger needs no bot branch.

-- +goose StatementBegin
DROP TRIGGER IF EXISTS search_fts_chat_au;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_chat_au AFTER UPDATE ON chat_messages BEGIN
    DELETE FROM search_fts WHERE kind = 'chat' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT '', NEW.body_md, 'chat', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.deleted_at IS NULL
          AND (NEW.kind = 'user' OR (NEW.kind = 'bot' AND NEW.gen_status = 'done'));
END;
-- +goose StatementEnd

-- Backfill done bot bubbles that streamed before this migration.
INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
    SELECT '', body_md, 'chat', id, community_id, created_at
    FROM chat_messages
    WHERE deleted_at IS NULL AND kind = 'bot' AND gen_status = 'done';

-- +goose Down

-- Drop the bot-indexed rows, then restore the user-only AU trigger from 00038.
DELETE FROM search_fts
    WHERE kind = 'chat'
      AND ref_id IN (SELECT id FROM chat_messages WHERE kind = 'bot');

-- +goose StatementBegin
DROP TRIGGER IF EXISTS search_fts_chat_au;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER search_fts_chat_au AFTER UPDATE ON chat_messages BEGIN
    DELETE FROM search_fts WHERE kind = 'chat' AND ref_id = OLD.id;
    INSERT INTO search_fts(title, body, kind, ref_id, community_id, created_at)
        SELECT '', NEW.body_md, 'chat', NEW.id, NEW.community_id, NEW.created_at
        WHERE NEW.deleted_at IS NULL AND NEW.kind = 'user';
END;
-- +goose StatementEnd
