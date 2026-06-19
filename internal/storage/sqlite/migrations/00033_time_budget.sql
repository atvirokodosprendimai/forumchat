-- +goose Up
-- +goose StatementBegin

-- Time accounting (Feature A): a per-community monthly time budget plus a
-- log of manual time entries. The whole community = one client; the budget
-- is a single recurring monthly figure (in minutes) that resets every
-- calendar month. Members see used / remaining; admins set the budget and
-- admins/mods log entries. Gated by TIME_ENABLED at route-mount level; the
-- tables always exist so flipping the flag never needs a schema migration.

CREATE TABLE time_budgets (
    community_id    TEXT PRIMARY KEY REFERENCES communities(id) ON DELETE CASCADE,
    monthly_minutes INTEGER NOT NULL DEFAULT 0,   -- 50h = 3000; 0 = unset
    updated_by      TEXT NOT NULL REFERENCES users(id),
    updated_at      INTEGER NOT NULL
);

-- One manual entry = "N minutes spent on <task>, on <date>". occurred_on is a
-- TEXT 'YYYY-MM-DD' so the current-month bucket is a pure substr() — no tz
-- math on unix ints. project_id is an optional tag for the breakdown; it
-- nulls out (not cascades) if the project is later deleted, so the logged
-- time is never lost.
CREATE TABLE time_entries (
    id           TEXT PRIMARY KEY,
    community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    project_id   TEXT REFERENCES projects(id) ON DELETE SET NULL,
    minutes      INTEGER NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    occurred_on  TEXT NOT NULL,            -- 'YYYY-MM-DD'
    created_by   TEXT NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_time_entries_month ON time_entries(community_id, occurred_on);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_time_entries_month;
DROP TABLE IF EXISTS time_entries;
DROP TABLE IF EXISTS time_budgets;

-- +goose StatementEnd
