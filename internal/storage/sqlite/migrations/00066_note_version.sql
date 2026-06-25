-- +goose Up
-- Collaborative editing: a monotonic version on each note. The server is the
-- single sequencer (SQLite is single-writer), so every merged edit bumps version
-- under the same read-modify-write transaction — concurrent edits from several
-- editors serialize cleanly and converge. Clients track the last version they
-- synced to detect when they're behind.
ALTER TABLE notes ADD COLUMN version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE notes DROP COLUMN version;
