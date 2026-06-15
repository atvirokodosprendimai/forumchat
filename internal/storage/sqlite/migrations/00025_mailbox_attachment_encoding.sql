-- +goose Up
-- +goose StatementBegin

-- Content-Transfer-Encoding for each email_ingest_attachment, read at
-- materialise time so Service.Materialise can base64-decode (or strip
-- quoted-printable padding) before writing the bytes to uploads.
-- Previously every attachment was saved raw which meant binary files
-- (SVG, PDF, images) landed as their base64 text envelope — downloads
-- looked corrupt.

ALTER TABLE email_ingest_attachment ADD COLUMN transfer_encoding TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE email_ingest_attachment DROP COLUMN transfer_encoding;

-- +goose StatementEnd
