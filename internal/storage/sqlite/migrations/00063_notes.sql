-- +goose Up
-- Community shared notes ("iNotes"). A member writes a note in markdown on a
-- dedicated page; it renders to sanitized HTML for reading. Two visibilities:
--   * 'public'  — listed community-wide, readable by any approved member.
--   * 'private' — NOT listed; readable only via an unguessable share_token link
--                 (the capability), or by the author / a moderator in the editor.
-- Sharing a note drops its URL into a chat channel; a private note's link carries
-- the token so the recipient can read it without it ever appearing in the list.
-- Editable repeatedly by the author OR a moderator/admin (unlike pastes, which
-- freeze on post). channel_id remembers the last channel shared to, for the
-- post-back default; ON DELETE SET NULL keeps the note alive if it is removed.
CREATE TABLE notes (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    channel_id   TEXT REFERENCES chat_channels(id) ON DELETE SET NULL,
    author_id    TEXT NOT NULL REFERENCES users(id),
    title        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',      -- markdown source (the edit form)
    body_html    TEXT NOT NULL DEFAULT '',      -- rendered + sanitized (the reader)
    visibility   TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public','private')),
    share_token  TEXT NOT NULL DEFAULT '',      -- capability token for the private link
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_notes_community ON notes(community_id);
CREATE INDEX idx_notes_author ON notes(author_id);
-- The token is the bearer capability for a private note; it must be unique so a
-- lookup resolves exactly one note. Partial so the empty default doesn't collide.
CREATE UNIQUE INDEX idx_notes_share_token ON notes(share_token) WHERE share_token <> '';

-- An inline comment anchored to the rendered HTML of a note. block_index is the
-- 0-based position of the top-level rendered block (the "line") the comment
-- attaches to; quote is the selected-text snippet for a range comment ('' = a
-- whole-block/line comment). After an edit shifts the blocks a comment whose
-- block_index no longer resolves is shown "orphaned" in the margin rather than
-- moved. resolved_at marks a comment closed; the row is kept for history.
CREATE TABLE note_comments (
    id           TEXT PRIMARY KEY,
    note_id      TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    author_id    TEXT NOT NULL REFERENCES users(id),
    block_index  INTEGER NOT NULL DEFAULT 0,
    quote        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    body_html    TEXT NOT NULL DEFAULT '',
    resolved_at  INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_note_comments_note ON note_comments(note_id);

-- +goose Down
DROP TABLE note_comments;
DROP TABLE notes;
