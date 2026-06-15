-- +goose Up
-- +goose StatementBegin

-- IMAP mailbox ingest. A single read-only IMAP account feeds the whole
-- instance; per-community filters route matched messages by their From:
-- address into a sorting inbox. Attachments are indexed at poll time
-- but their bytes are NOT downloaded until a user clicks "move to project"
-- in the global /inbox UI. See eidos/spec - mailbox - imap-ingest-...
--
-- Feature is gated by MAILBOX_ENABLED env at the route / worker mount
-- level. Tables always exist so toggling the flag never needs a schema
-- migration.

CREATE TABLE mailbox_account (
    id          TEXT PRIMARY KEY,
    host        TEXT NOT NULL,
    port        INTEGER NOT NULL,
    username    TEXT NOT NULL,
    password    TEXT NOT NULL,           -- plaintext in v1; see spec Notes
    tls_mode    TEXT NOT NULL DEFAULT 'tls',
    last_poll_at INTEGER,
    last_error  TEXT,
    created_at  INTEGER NOT NULL
);

CREATE TABLE mailbox_folder (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL REFERENCES mailbox_account(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    uidvalidity   INTEGER NOT NULL DEFAULT 0,
    last_uid      INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_seen_at  INTEGER,
    last_error    TEXT
);
CREATE UNIQUE INDEX idx_mailbox_folder_account_name ON mailbox_folder(account_id, name);

CREATE TABLE community_mail_filter (
    id            TEXT PRIMARY KEY,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK(kind IN ('domain','address')),
    pattern       TEXT NOT NULL,         -- '@example.com' OR 'alice@example.com', lowercase
    to_issue      INTEGER NOT NULL DEFAULT 0,
    created_by    TEXT NOT NULL REFERENCES users(id),
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_community_mail_filter_lookup ON community_mail_filter(kind, pattern);
CREATE INDEX idx_community_mail_filter_community ON community_mail_filter(community_id, created_at DESC);

CREATE TABLE email_ingest (
    id                TEXT PRIMARY KEY,
    folder_id         TEXT NOT NULL REFERENCES mailbox_folder(id) ON DELETE CASCADE,
    uid               INTEGER NOT NULL,
    uidvalidity       INTEGER NOT NULL,
    message_id        TEXT NOT NULL DEFAULT '',
    from_addr         TEXT NOT NULL,
    from_name         TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    received_at       INTEGER NOT NULL,
    community_id      TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    status            TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','consumed','discarded')),
    matched_filter_id TEXT REFERENCES community_mail_filter(id) ON DELETE SET NULL,
    created_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_email_ingest_uid ON email_ingest(folder_id, uid, uidvalidity);
CREATE INDEX idx_email_ingest_queue ON email_ingest(community_id, status, received_at DESC, id DESC);

CREATE TABLE email_ingest_attachment (
    id                  TEXT PRIMARY KEY,
    ingest_id           TEXT NOT NULL REFERENCES email_ingest(id) ON DELETE CASCADE,
    filename            TEXT NOT NULL,
    mime                TEXT NOT NULL,
    size_bytes          INTEGER NOT NULL,
    mime_part_id        TEXT NOT NULL,         -- '2' or '2.1' etc, used in BODY.PEEK[2.1]
    upload_id           TEXT REFERENCES uploads(id) ON DELETE SET NULL,
    moved_to_project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    moved_category      TEXT,
    moved_at            INTEGER,
    created_at          INTEGER NOT NULL
);
CREATE INDEX idx_email_ingest_attachment_ingest ON email_ingest_attachment(ingest_id);

CREATE TABLE email_ingest_issue (
    ingest_id   TEXT PRIMARY KEY REFERENCES email_ingest(id) ON DELETE CASCADE,
    issue_id    TEXT NOT NULL REFERENCES project_issues(id) ON DELETE CASCADE,
    created_at  INTEGER NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS email_ingest_issue;
DROP INDEX IF EXISTS idx_email_ingest_attachment_ingest;
DROP TABLE IF EXISTS email_ingest_attachment;
DROP INDEX IF EXISTS idx_email_ingest_queue;
DROP INDEX IF EXISTS idx_email_ingest_uid;
DROP TABLE IF EXISTS email_ingest;
DROP INDEX IF EXISTS idx_community_mail_filter_community;
DROP INDEX IF EXISTS idx_community_mail_filter_lookup;
DROP TABLE IF EXISTS community_mail_filter;
DROP INDEX IF EXISTS idx_mailbox_folder_account_name;
DROP TABLE IF EXISTS mailbox_folder;
DROP TABLE IF EXISTS mailbox_account;

-- +goose StatementEnd
