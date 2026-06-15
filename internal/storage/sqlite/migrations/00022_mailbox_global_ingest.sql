-- +goose Up
-- +goose StatementBegin

-- email_ingest used to NOT NULL on community_id which meant the poll
-- worker had to drop every unmatched email on the floor — defeating
-- the point of the click-sender-attach popover (you can't pick a
-- sender to file if the sender's mail never landed).
--
-- Now we ingest everything. NULL community_id == "unassigned" and the
-- global inbox renders a third pill alongside the per-community ones.
-- Filters become routing rules (assign to community), not gates.
--
-- SQLite can't DROP NOT NULL in place; recreate the table.

PRAGMA foreign_keys = OFF;

CREATE TABLE email_ingest_new (
    id                TEXT PRIMARY KEY,
    folder_id         TEXT NOT NULL REFERENCES mailbox_folder(id) ON DELETE CASCADE,
    uid               INTEGER NOT NULL,
    uidvalidity       INTEGER NOT NULL,
    message_id        TEXT NOT NULL DEFAULT '',
    from_addr         TEXT NOT NULL,
    from_name         TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    body_text         TEXT NOT NULL DEFAULT '',
    received_at       INTEGER NOT NULL,
    community_id      TEXT REFERENCES communities(id) ON DELETE SET NULL,
    status            TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','consumed','discarded')),
    matched_filter_id TEXT REFERENCES community_mail_filter(id) ON DELETE SET NULL,
    created_at        INTEGER NOT NULL
);

INSERT INTO email_ingest_new
    (id, folder_id, uid, uidvalidity, message_id, from_addr, from_name, subject, body_text, received_at, community_id, status, matched_filter_id, created_at)
SELECT id, folder_id, uid, uidvalidity, message_id, from_addr, from_name, subject, body_text, received_at, community_id, status, matched_filter_id, created_at
FROM email_ingest;

DROP TABLE email_ingest;
ALTER TABLE email_ingest_new RENAME TO email_ingest;

CREATE UNIQUE INDEX idx_email_ingest_uid ON email_ingest(folder_id, uid, uidvalidity);
CREATE INDEX idx_email_ingest_queue ON email_ingest(community_id, status, received_at DESC, id DESC);
CREATE INDEX idx_email_ingest_unassigned ON email_ingest(status, received_at DESC, id DESC) WHERE community_id IS NULL;

PRAGMA foreign_keys = ON;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

PRAGMA foreign_keys = OFF;

CREATE TABLE email_ingest_strict (
    id                TEXT PRIMARY KEY,
    folder_id         TEXT NOT NULL REFERENCES mailbox_folder(id) ON DELETE CASCADE,
    uid               INTEGER NOT NULL,
    uidvalidity       INTEGER NOT NULL,
    message_id        TEXT NOT NULL DEFAULT '',
    from_addr         TEXT NOT NULL,
    from_name         TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    body_text         TEXT NOT NULL DEFAULT '',
    received_at       INTEGER NOT NULL,
    community_id      TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    status            TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','consumed','discarded')),
    matched_filter_id TEXT REFERENCES community_mail_filter(id) ON DELETE SET NULL,
    created_at        INTEGER NOT NULL
);

INSERT INTO email_ingest_strict
SELECT * FROM email_ingest WHERE community_id IS NOT NULL;

DROP INDEX IF EXISTS idx_email_ingest_unassigned;
DROP INDEX IF EXISTS idx_email_ingest_queue;
DROP INDEX IF EXISTS idx_email_ingest_uid;
DROP TABLE email_ingest;
ALTER TABLE email_ingest_strict RENAME TO email_ingest;

CREATE UNIQUE INDEX idx_email_ingest_uid ON email_ingest(folder_id, uid, uidvalidity);
CREATE INDEX idx_email_ingest_queue ON email_ingest(community_id, status, received_at DESC, id DESC);

PRAGMA foreign_keys = ON;

-- +goose StatementEnd
