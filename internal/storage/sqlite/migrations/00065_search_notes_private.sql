-- +goose Up
-- Author-private full-text search. A member can full-text search their OWN
-- private notes. These rows must NEVER surface in community-wide search
-- (search_fts is public-only, 00038/00064), so they live in a SEPARATE FTS5
-- index keyed by author_id and queried scoped to (community_id, author_id) =
-- the viewer. A note is in exactly one of the two indexes: public → search_fts,
-- private → note_private_fts.
CREATE VIRTUAL TABLE note_private_fts USING fts5(
    title,
    body,
    ref_id UNINDEXED,
    author_id UNINDEXED,
    community_id UNINDEXED,
    created_at UNINDEXED
);

-- Backfill existing private notes.
INSERT INTO note_private_fts(title, body, ref_id, author_id, community_id, created_at)
    SELECT title, body, id, author_id, community_id, created_at
    FROM notes WHERE visibility = 'private';

-- +goose StatementBegin
CREATE TRIGGER note_private_fts_ai AFTER INSERT ON notes
WHEN NEW.visibility = 'private' BEGIN
    INSERT INTO note_private_fts(title, body, ref_id, author_id, community_id, created_at)
        VALUES (NEW.title, NEW.body, NEW.id, NEW.author_id, NEW.community_id, NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER note_private_fts_ad AFTER DELETE ON notes BEGIN
    DELETE FROM note_private_fts WHERE ref_id = OLD.id;
END;
-- +goose StatementEnd
-- A note flips private<->public and edits its body on UPDATE; rebuild the row
-- iff it is currently private (a note going public leaves this index and enters
-- search_fts via 00064's trigger).
-- +goose StatementBegin
CREATE TRIGGER note_private_fts_au AFTER UPDATE ON notes BEGIN
    DELETE FROM note_private_fts WHERE ref_id = OLD.id;
    INSERT INTO note_private_fts(title, body, ref_id, author_id, community_id, created_at)
        SELECT NEW.title, NEW.body, NEW.id, NEW.author_id, NEW.community_id, NEW.created_at
        WHERE NEW.visibility = 'private';
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS note_private_fts_au;
DROP TRIGGER IF EXISTS note_private_fts_ad;
DROP TRIGGER IF EXISTS note_private_fts_ai;
DROP TABLE note_private_fts;
