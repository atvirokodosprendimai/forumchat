-- +goose Up

-- Agile-ify the project checklist: a todo gains a 3-state status
-- (todo | in_progress | done), an explicit completion timestamp, and an
-- optional assignee. `done` stays as the source of truth's mirror so the
-- index-card progress count (SELECT … WHERE done = 1) and the existing
-- checkbox toggle keep working unchanged — every status write syncs it.
ALTER TABLE project_todos ADD COLUMN status TEXT NOT NULL DEFAULT 'todo';
ALTER TABLE project_todos ADD COLUMN completed_at INTEGER;
ALTER TABLE project_todos ADD COLUMN assignee_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;

-- Backfill: rows already marked done become status='done' and inherit
-- their last-updated time as the completion stamp (best available proxy).
UPDATE project_todos SET status = 'done', completed_at = updated_at WHERE done = 1;

CREATE INDEX idx_project_todos_assignee ON project_todos(assignee_user_id);

-- +goose Down
DROP INDEX IF EXISTS idx_project_todos_assignee;
ALTER TABLE project_todos DROP COLUMN assignee_user_id;
ALTER TABLE project_todos DROP COLUMN completed_at;
ALTER TABLE project_todos DROP COLUMN status;
