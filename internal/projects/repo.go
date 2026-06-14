package projects

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Repo is the SQLite-backed persistence layer for projects.
type Repo struct{ DB *sql.DB }

// NewRepo wraps a connection.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// ListActiveForCommunity returns active (non-archived) projects ordered
// most-recently-updated first. Aggregates todo / attachment / comment
// counts in one query to avoid N+1 on the index page.
func (r *Repo) ListActiveForCommunity(ctx context.Context, communityID string) ([]IndexRow, error) {
	return r.listForCommunity(ctx, communityID, false)
}

// ListArchivedForCommunity is the same shape but returns archived rows
// ordered most-recently-archived first.
func (r *Repo) ListArchivedForCommunity(ctx context.Context, communityID string) ([]IndexRow, error) {
	return r.listForCommunity(ctx, communityID, true)
}

func (r *Repo) listForCommunity(ctx context.Context, communityID string, archived bool) ([]IndexRow, error) {
	where := "p.archived_at IS NULL"
	order := "p.updated_at DESC"
	if archived {
		where = "p.archived_at IS NOT NULL"
		order = "p.archived_at DESC"
	}
	q := fmt.Sprintf(`
		SELECT p.id, p.community_id, p.creator_user_id, p.title,
		       p.description_md, p.description_html, p.archived_at,
		       p.created_at, p.updated_at,
		       (SELECT COUNT(*) FROM project_todos t WHERE t.project_id = p.id) AS todo_total,
		       (SELECT COUNT(*) FROM project_todos t WHERE t.project_id = p.id AND t.done = 1) AS todo_done,
		       (SELECT COUNT(*) FROM project_attachments a WHERE a.project_id = p.id) AS att_count,
		       (SELECT COUNT(*) FROM project_comments c WHERE c.project_id = p.id AND c.deleted_at IS NULL) AS cmt_count
		FROM projects p
		WHERE p.community_id = ? AND %s
		ORDER BY %s`, where, order)
	rows, err := r.DB.QueryContext(ctx, q, communityID)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var row IndexRow
		var arch sql.NullInt64
		var cAt, uAt int64
		if err := rows.Scan(&row.ID, &row.CommunityID, &row.CreatorUserID, &row.Title,
			&row.DescriptionMD, &row.DescriptionHTML, &arch, &cAt, &uAt,
			&row.TodoTotal, &row.TodoDone, &row.AttachmentCount, &row.CommentCount); err != nil {
			return nil, err
		}
		row.CreatedAt = time.UnixMilli(cAt).UTC()
		row.UpdatedAt = time.UnixMilli(uAt).UTC()
		if arch.Valid {
			t := time.UnixMilli(arch.Int64).UTC()
			row.ArchivedAt = &t
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ByID loads one project. Returns sql.ErrNoRows wrapped if missing.
func (r *Repo) ByID(ctx context.Context, id string) (Project, error) {
	var p Project
	var arch sql.NullInt64
	var cAt, uAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, creator_user_id, title,
		       description_md, description_html, archived_at, created_at, updated_at
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.CommunityID, &p.CreatorUserID, &p.Title,
		&p.DescriptionMD, &p.DescriptionHTML, &arch, &cAt, &uAt)
	if err != nil {
		return Project{}, err
	}
	p.CreatedAt = time.UnixMilli(cAt).UTC()
	p.UpdatedAt = time.UnixMilli(uAt).UTC()
	if arch.Valid {
		t := time.UnixMilli(arch.Int64).UTC()
		p.ArchivedAt = &t
	}
	return p, nil
}

// Insert persists a fresh project row.
func (r *Repo) Insert(ctx context.Context, p Project) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO projects
		  (id, community_id, creator_user_id, title, description_md, description_html, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.CommunityID, p.CreatorUserID, p.Title,
		p.DescriptionMD, p.DescriptionHTML,
		p.CreatedAt.UnixMilli(), p.UpdatedAt.UnixMilli())
	return err
}

// UpdateTitle and UpdateDescription bump updated_at as a side-effect so
// the index page re-sorts correctly without callers having to remember.
func (r *Repo) UpdateTitle(ctx context.Context, id, title string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE projects SET title = ?, updated_at = ? WHERE id = ?`,
		title, now.UnixMilli(), id)
	return err
}

func (r *Repo) UpdateDescription(ctx context.Context, id, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE projects SET description_md = ?, description_html = ?, updated_at = ? WHERE id = ?`,
		md, html, now.UnixMilli(), id)
	return err
}

// SetArchived flips the archived_at timestamp. nil clears it.
func (r *Repo) SetArchived(ctx context.Context, id string, at *time.Time, now time.Time) error {
	var arch any
	if at != nil {
		arch = at.UnixMilli()
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE projects SET archived_at = ?, updated_at = ? WHERE id = ?`,
		arch, now.UnixMilli(), id)
	return err
}

// Delete hard-deletes the project; FKs cascade rows in todos /
// attachments / comments.
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

// ListTodos returns a project's checklist in sort_order, then create
// order, ascending.
func (r *Repo) ListTodos(ctx context.Context, projectID string) ([]Todo, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, project_id, body, done, sort_order, created_by, created_at, updated_at
		FROM project_todos
		WHERE project_id = ?
		ORDER BY sort_order ASC, created_at ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		var done int
		var cAt, uAt int64
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Body, &done, &t.SortOrder,
			&t.CreatedBy, &cAt, &uAt); err != nil {
			return nil, err
		}
		t.Done = done == 1
		t.CreatedAt = time.UnixMilli(cAt).UTC()
		t.UpdatedAt = time.UnixMilli(uAt).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// MaxTodoSortOrder returns the highest sort_order in a project, or -1
// if there are none. New rows append at max+1 so they land at the end.
func (r *Repo) MaxTodoSortOrder(ctx context.Context, projectID string) (int, error) {
	var n sql.NullInt64
	err := r.DB.QueryRowContext(ctx,
		`SELECT MAX(sort_order) FROM project_todos WHERE project_id = ?`,
		projectID).Scan(&n)
	if err != nil {
		return 0, err
	}
	if !n.Valid {
		return -1, nil
	}
	return int(n.Int64), nil
}

// InsertTodo persists a fresh checklist row.
func (r *Repo) InsertTodo(ctx context.Context, t Todo) error {
	done := 0
	if t.Done {
		done = 1
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_todos (id, project_id, body, done, sort_order, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Body, done, t.SortOrder, t.CreatedBy,
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

// UpdateTodoBody changes the text and bumps updated_at.
func (r *Repo) UpdateTodoBody(ctx context.Context, id, body string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_todos SET body = ?, updated_at = ? WHERE id = ?`,
		body, now.UnixMilli(), id)
	return err
}

// ToggleTodoDone flips the done flag for one row and bumps updated_at.
func (r *Repo) ToggleTodoDone(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_todos SET done = CASE done WHEN 0 THEN 1 ELSE 0 END, updated_at = ? WHERE id = ?`,
		now.UnixMilli(), id)
	return err
}

// DeleteTodo removes a row outright.
func (r *Repo) DeleteTodo(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM project_todos WHERE id = ?`, id)
	return err
}

// TodoByID loads one row.
func (r *Repo) TodoByID(ctx context.Context, id string) (Todo, error) {
	var t Todo
	var done int
	var cAt, uAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, body, done, sort_order, created_by, created_at, updated_at
		FROM project_todos WHERE id = ?`, id).Scan(
		&t.ID, &t.ProjectID, &t.Body, &done, &t.SortOrder,
		&t.CreatedBy, &cAt, &uAt)
	if err != nil {
		return Todo{}, err
	}
	t.Done = done == 1
	t.CreatedAt = time.UnixMilli(cAt).UTC()
	t.UpdatedAt = time.UnixMilli(uAt).UTC()
	return t, nil
}

// ActivityEvent is one entry in the audit panel.
type ActivityEvent struct {
	Kind string // "comment" | "attachment" | "todo" | "project"
	At   time.Time
}

// RecentActivity returns up to N most-recent timestamps from the
// project's child tables UNIONed with the project's own updated_at.
// Used by the activity sidebar — no separate audit table needed.
func (r *Repo) RecentActivity(ctx context.Context, projectID string, limit int) ([]ActivityEvent, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT * FROM (
			SELECT 'comment'    AS kind, created_at AS at FROM project_comments     WHERE project_id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT 'attachment' AS kind, created_at AS at FROM project_attachments  WHERE project_id = ?
			UNION ALL
			SELECT 'todo'       AS kind, created_at AS at FROM project_todos        WHERE project_id = ?
			UNION ALL
			SELECT 'project'    AS kind, updated_at AS at FROM projects             WHERE id = ?
		)
		ORDER BY at DESC LIMIT ?`,
		projectID, projectID, projectID, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	defer rows.Close()
	var out []ActivityEvent
	for rows.Next() {
		var ev ActivityEvent
		var at int64
		if err := rows.Scan(&ev.Kind, &at); err != nil {
			return nil, err
		}
		ev.At = time.UnixMilli(at).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListAttachments returns all attachments for a project, most-recent
// first.
func (r *Repo) ListAttachments(ctx context.Context, projectID string) ([]Attachment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, project_id, upload_id, filename, mime, size_bytes, uploader_id, created_at
		FROM project_attachments
		WHERE project_id = ?
		ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var cAt int64
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.UploadID, &a.Filename,
			&a.MIME, &a.SizeBytes, &a.UploaderID, &cAt); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(cAt).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAttachment persists one row pointing at an uploads.id.
func (r *Repo) InsertAttachment(ctx context.Context, a Attachment) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_attachments
		  (id, project_id, upload_id, filename, mime, size_bytes, uploader_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectID, a.UploadID, a.Filename, a.MIME, a.SizeBytes,
		a.UploaderID, a.CreatedAt.UnixMilli())
	return err
}

// AttachmentByID loads one attachment row.
func (r *Repo) AttachmentByID(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, upload_id, filename, mime, size_bytes, uploader_id, created_at
		FROM project_attachments WHERE id = ?`, id).Scan(
		&a.ID, &a.ProjectID, &a.UploadID, &a.Filename, &a.MIME,
		&a.SizeBytes, &a.UploaderID, &cAt)
	if err != nil {
		return Attachment{}, err
	}
	a.CreatedAt = time.UnixMilli(cAt).UTC()
	return a, nil
}

// DeleteAttachment removes the project_attachments row. Caller is
// responsible for cleaning up the underlying uploads row + file via
// uploads.Store.Delete if the policy calls for it.
func (r *Repo) DeleteAttachment(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM project_attachments WHERE id = ?`, id)
	return err
}

// ListComments returns a project's comments in created_at ascending,
// excluding soft-deleted rows. Forum-style edit_at / deleted_at apply.
func (r *Repo) ListComments(ctx context.Context, projectID string) ([]Comment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, project_id, author_id, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_comments
		WHERE project_id = ?
		ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		var edited, deleted sql.NullInt64
		var cAt int64
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.AuthorID, &c.BodyMD,
			&c.BodyHTML, &edited, &deleted, &cAt); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(cAt).UTC()
		if edited.Valid {
			t := time.UnixMilli(edited.Int64).UTC()
			c.EditedAt = &t
		}
		if deleted.Valid {
			t := time.UnixMilli(deleted.Int64).UTC()
			c.DeletedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertComment persists one new comment.
func (r *Repo) InsertComment(ctx context.Context, c Comment) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_comments (id, project_id, author_id, body_md, body_html, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.ProjectID, c.AuthorID, c.BodyMD, c.BodyHTML, c.CreatedAt.UnixMilli())
	return err
}

// UpdateComment replaces body + bumps edited_at.
func (r *Repo) UpdateComment(ctx context.Context, id, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_comments SET body_md = ?, body_html = ?, edited_at = ? WHERE id = ?`,
		md, html, now.UnixMilli(), id)
	return err
}

// SoftDeleteComment stamps deleted_at — preserves the row so existing
// references (e.g. nested replies in future) stay resolvable.
func (r *Repo) SoftDeleteComment(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_comments SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.UnixMilli(), id)
	return err
}

// CommentByID loads one row, including soft-deleted ones (so the
// service can check the author/grace before re-acting).
func (r *Repo) CommentByID(ctx context.Context, id string) (Comment, error) {
	var c Comment
	var edited, deleted sql.NullInt64
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, author_id, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_comments WHERE id = ?`, id).Scan(
		&c.ID, &c.ProjectID, &c.AuthorID, &c.BodyMD, &c.BodyHTML,
		&edited, &deleted, &cAt)
	if err != nil {
		return Comment{}, err
	}
	c.CreatedAt = time.UnixMilli(cAt).UTC()
	if edited.Valid {
		t := time.UnixMilli(edited.Int64).UTC()
		c.EditedAt = &t
	}
	if deleted.Valid {
		t := time.UnixMilli(deleted.Int64).UTC()
		c.DeletedAt = &t
	}
	return c, nil
}

// ReorderTodos applies a new (id -> sort_order) mapping inside one
// transaction. Callers pass the full desired order so we don't need to
// fiddle with fractional indexes.
func (r *Repo) ReorderTodos(ctx context.Context, projectID string, order []string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE project_todos SET sort_order = ? WHERE id = ? AND project_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range order {
		if _, err := stmt.ExecContext(ctx, i, id, projectID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
