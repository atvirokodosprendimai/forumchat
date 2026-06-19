-- +goose Up
-- +goose StatementBegin

-- Personal work log (Feature B): a global per-user Start/Stop timer plus a
-- journal of past sessions. Not community-scoped — it follows the user across
-- communities. ended_at NULL means the timer is currently running; note is
-- filled when the user stops and answers "what did you do?". The partial
-- unique index enforces at-most-one running timer per user at the DB level,
-- so a double-Start can never create two live sessions.

CREATE TABLE timer_sessions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,                 -- NULL = running
    note       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_timer_active ON timer_sessions(user_id) WHERE ended_at IS NULL;
CREATE INDEX idx_timer_sessions_user ON timer_sessions(user_id, started_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_timer_sessions_user;
DROP INDEX IF EXISTS uq_timer_active;
DROP TABLE IF EXISTS timer_sessions;

-- +goose StatementEnd
