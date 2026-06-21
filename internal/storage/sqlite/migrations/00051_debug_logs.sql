-- +goose Up
-- +goose StatementBegin

-- Platform-wide debug log. A super-admin toggle (in-memory, off by default,
-- resets on restart) gates writes; when on, integration call sites (webhooks
-- inbound/outbound, etc.) record their raw payloads here for debugging. Read
-- and cleared only from the /superadmin/debug surface. No FK to communities:
-- entries are cross-community platform diagnostics and outlive any row they
-- reference.
CREATE TABLE debug_logs (
    id         TEXT PRIMARY KEY,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    source     TEXT NOT NULL,             -- 'webhook' (room for more later)
    event      TEXT NOT NULL,             -- 'inbound' | 'outbound' | ...
    summary    TEXT NOT NULL DEFAULT '',  -- short human label (provider, target, status)
    payload    TEXT NOT NULL DEFAULT '',  -- raw body, possibly truncated
    meta       TEXT NOT NULL DEFAULT ''   -- optional JSON key/value context
);

CREATE INDEX idx_debug_logs_created_at ON debug_logs(created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_debug_logs_created_at;
DROP TABLE IF EXISTS debug_logs;

-- +goose StatementEnd
