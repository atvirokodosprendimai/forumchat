-- +goose Up
-- filename is the user-supplied display name at upload time. Empty
-- string for sources that have no name (pasted data: URLs, server-
-- minted artifacts). Used to:
--   1. Preserve the original name in download Content-Disposition so
--      "invoice-2026-04.pdf" lands as itself instead of "<sha>.bin".
--   2. Seed the "extract to project / new issue" title in the chat
--      attachments feature (prefilled from filename).
ALTER TABLE uploads ADD COLUMN filename TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE uploads DROP COLUMN filename;
