-- +goose Up
-- +goose StatementBegin

-- community_exports backs the owner-initiated "download ALL my data" flow
-- (eidos/spec - data-export …). An owner requests an export; a background
-- worker (ExportWorker) drains the pending row, builds a ZIP of every business
-- function's data + media under <uploads>/exports/<id>.zip, then stamps it
-- 'ready' with a high-entropy capability token and a 7-day expiry. The download
-- URL is the signed link; after expiry a sweep deletes the file and marks the
-- row 'expired', so a new request is required.
--
-- community_id cascades: a deleted community's export rows (and its on-disk zip,
-- removed by Provision.Delete's blob purge path is NOT involved here — the zip
-- lives under exports/, swept on expiry) are meaningless. requested_by is a soft
-- stamp (ON DELETE SET NULL) so an export survives the requester being erased.
CREATE TABLE community_exports (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    requested_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
    status        TEXT NOT NULL DEFAULT 'pending',  -- pending | building | ready | failed | expired
    token         TEXT NOT NULL DEFAULT '',         -- capability for the download URL (set on ready)
    rel_path      TEXT NOT NULL DEFAULT '',         -- zip path relative to the exports dir
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    requested_at  INTEGER NOT NULL,
    ready_at      INTEGER,
    expires_at    INTEGER,                          -- ready_at + 7d
    created_at    INTEGER NOT NULL
);

CREATE INDEX idx_community_exports_community ON community_exports(community_id, created_at);
CREATE INDEX idx_community_exports_status ON community_exports(status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_community_exports_status;
DROP INDEX IF EXISTS idx_community_exports_community;
DROP TABLE IF EXISTS community_exports;

-- +goose StatementEnd
