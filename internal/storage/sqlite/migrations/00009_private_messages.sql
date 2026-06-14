-- +goose Up
-- +goose StatementBegin

CREATE TABLE private_threads (
    id                      TEXT PRIMARY KEY,
    initiator_user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recipient_user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status                  TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','declined')),
    source_community_id     TEXT REFERENCES communities(id) ON DELETE SET NULL,
    source_chat_message_id  TEXT,
    last_message_at         INTEGER NOT NULL,
    created_at              INTEGER NOT NULL
);
CREATE INDEX idx_pm_threads_recipient ON private_threads(recipient_user_id, status, last_message_at DESC);
CREATE INDEX idx_pm_threads_initiator ON private_threads(initiator_user_id, status, last_message_at DESC);

CREATE TABLE private_messages (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL REFERENCES private_threads(id) ON DELETE CASCADE,
    author_user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body            TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_pm_messages_thread ON private_messages(thread_id, created_at);

CREATE TABLE private_thread_reads (
    thread_id     TEXT NOT NULL REFERENCES private_threads(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_read_at  INTEGER NOT NULL,
    PRIMARY KEY (thread_id, user_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS private_thread_reads;
DROP INDEX IF EXISTS idx_pm_messages_thread;
DROP TABLE IF EXISTS private_messages;
DROP INDEX IF EXISTS idx_pm_threads_initiator;
DROP INDEX IF EXISTS idx_pm_threads_recipient;
DROP TABLE IF EXISTS private_threads;

-- +goose StatementEnd
