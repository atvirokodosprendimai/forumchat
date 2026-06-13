-- +goose Up
-- +goose StatementBegin

-- Backfill: existing thread_announce chat messages were written with the
-- pre-multi-community URL pattern (BaseURL/forum/{id}). New rows after
-- commit da46bd4 use BaseURL/c/{slug}/forum/{id}. Rewrite the old rows so
-- the historical announces actually link to the right thread.

UPDATE chat_messages
SET body_html = REPLACE(
        body_html,
        '/forum/' || ref_thread_id,
        '/c/' || (
            SELECT c.slug FROM threads t
            JOIN communities c ON c.id = t.community_id
            WHERE t.id = chat_messages.ref_thread_id
        ) || '/forum/' || ref_thread_id
    ),
    body_md = REPLACE(
        body_md,
        '/forum/' || ref_thread_id,
        '/c/' || (
            SELECT c.slug FROM threads t
            JOIN communities c ON c.id = t.community_id
            WHERE t.id = chat_messages.ref_thread_id
        ) || '/forum/' || ref_thread_id
    )
WHERE kind = 'thread_announce'
  AND ref_thread_id IS NOT NULL
  AND body_html LIKE '%/forum/' || ref_thread_id || '%'
  AND body_html NOT LIKE '%/c/%/forum/' || ref_thread_id || '%';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- No-op: the rewrite is content-only and not reversible without storing
-- the original strings somewhere.

-- +goose StatementEnd
