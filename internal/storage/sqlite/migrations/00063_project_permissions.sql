-- +goose Up

-- Opt-in per-project permissions. All three columns default so EVERY
-- existing project keeps today's behaviour: needs_perms=0 means "every
-- approved member reads + writes" exactly as before — these columns are
-- inert until an owner/admin flips needs_perms on.
--
--   needs_perms   master switch. 0 = legacy open project (default).
--   visibility    when needs_perms=1: 'community' (all members may read)
--                 or 'restricted' (only ACL'd members + creator/admin) —
--                 the "hide" switch (e.g. the email-drop Inbox).
--   member_access when needs_perms=1 AND visibility='community': the
--                 community-wide default for members — 'read' (read-only,
--                 the default) or 'write'. Per-person grants override it.
ALTER TABLE projects ADD COLUMN needs_perms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN visibility TEXT NOT NULL DEFAULT 'community';
ALTER TABLE projects ADD COLUMN member_access TEXT NOT NULL DEFAULT 'read';

-- Per-person access control list. A row grants one user 'read' or 'write'
-- on one project. In 'restricted' visibility these rows are the ONLY way a
-- non-creator/non-admin member sees the project; in 'community' visibility
-- they override the member_access default for that user.
CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access     TEXT NOT NULL DEFAULT 'read',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_project_members_user ON project_members(user_id);

-- +goose Down
DROP INDEX IF EXISTS idx_project_members_user;
DROP TABLE IF EXISTS project_members;
ALTER TABLE projects DROP COLUMN member_access;
ALTER TABLE projects DROP COLUMN visibility;
ALTER TABLE projects DROP COLUMN needs_perms;
