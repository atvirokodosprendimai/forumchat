-- +goose Up

-- Per-note "request to edit" ACL. A public note is editable only by its
-- author + community mods/admins (notes.CanManage); the collaborative editor
-- is therefore unreachable for a regular member, even though they can read and
-- comment. This table lets a member request edit rights and an editor grant
-- them, turning the diff-sync collaboration into something the whole community
-- can opt into — one note at a time, least-privilege.
--
--   status = 'pending'  the member asked; awaiting an editor's decision.
--   status = 'granted'  approved → the user is now a collaborator (CanEdit).
--
-- Decline (of a pending row) and revoke (of a granted row) are both a plain
-- DELETE — no row means "no relationship", so the member may ask again later.
-- UNIQUE(note_id, user_id) keeps it to one row per (note, member): an
-- INSERT ... ON CONFLICT DO NOTHING makes a repeat request idempotent and can
-- never downgrade an existing grant back to pending.
CREATE TABLE note_edit_requests (
    id           TEXT PRIMARY KEY,
    note_id      TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status       TEXT NOT NULL DEFAULT 'pending',
    message      TEXT NOT NULL DEFAULT '',
    requested_at INTEGER NOT NULL,
    decided_at   INTEGER,
    decided_by   TEXT REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (note_id, user_id)
);

-- The note page loads its pending requests + granted collaborators by note_id;
-- CanEdit resolves a note's granted set by (note_id, status='granted').
CREATE INDEX idx_note_edit_requests_note ON note_edit_requests(note_id, status);

-- +goose Down
DROP INDEX IF EXISTS idx_note_edit_requests_note;
DROP TABLE IF EXISTS note_edit_requests;
