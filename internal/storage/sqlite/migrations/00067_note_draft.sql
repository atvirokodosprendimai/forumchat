-- +goose Up
-- Collaborative editing operates on a shared DRAFT, published to `body` on Save.
-- Keeping the live draft separate from the published `body` means:
--   * in-progress edits stay OUT of the public FTS/RAG indexes (00064/00065 track
--     `body`) and out of the rendered reader (`body_html`) until an editor saves;
--   * Save publishes the draft and never clobbers a concurrent editor — the draft
--     is the single canonical that the diff-merge owns.
-- Backfill draft_body = body so existing notes open with their current content.
ALTER TABLE notes ADD COLUMN draft_body TEXT NOT NULL DEFAULT '';
UPDATE notes SET draft_body = body;

-- +goose Down
ALTER TABLE notes DROP COLUMN draft_body;
