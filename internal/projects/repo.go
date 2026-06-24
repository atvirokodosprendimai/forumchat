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

// RefRow is one match from SearchRefs (a project, issue, or discussion thread).
type RefRow struct {
	Kind  string // project | issue | discussion
	ID    string
	Title string
}

// SearchRefs finds projects, issues, and discussion threads in a community whose
// title/subject matches q (case-insensitive substring). Powers the agent
// composer's $-reference autocomplete.
func (r *Repo) SearchRefs(ctx context.Context, communityID, q string, limit int) []RefRow {
	like := "%" + likeEscape(q) + "%"
	var out []RefRow
	add := func(kind, query string) {
		rows, err := r.DB.QueryContext(ctx, query, communityID, like, limit)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ref RefRow
			ref.Kind = kind
			if err := rows.Scan(&ref.ID, &ref.Title); err == nil && ref.Title != "" {
				out = append(out, ref)
			}
		}
	}
	add("project", `SELECT id, title FROM projects
		WHERE community_id = ? AND title LIKE ? ESCAPE '\' ORDER BY title LIMIT ?`)
	add("issue", `SELECT i.id, i.title FROM project_issues i JOIN projects p ON i.project_id = p.id
		WHERE p.community_id = ? AND i.title LIKE ? ESCAPE '\' ORDER BY i.updated_at DESC LIMIT ?`)
	add("discussion", `SELECT t.id, t.subject FROM project_discussion_threads t JOIN projects p ON t.project_id = p.id
		WHERE p.community_id = ? AND t.subject LIKE ? ESCAPE '\' AND t.deleted_at IS NULL LIMIT ?`)
	return out
}

// likeEscape escapes LIKE wildcards so user input matches literally.
func likeEscape(s string) string {
	r := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			r = append(r, '\\', s[i])
		default:
			r = append(r, s[i])
		}
	}
	return string(r)
}

// ListActiveForCommunity returns active (non-archived) projects ordered
// most-recently-updated first. Aggregates todo / attachment / comment
// counts in one query to avoid N+1 on the index page. UNFILTERED — used by
// find-or-create + move pickers where the caller already owns the context.
func (r *Repo) ListActiveForCommunity(ctx context.Context, communityID string) ([]IndexRow, error) {
	return r.listForCommunity(ctx, communityID, false, nil)
}

// ListArchivedForCommunity is the same shape but returns archived rows
// ordered most-recently-archived first.
func (r *Repo) ListArchivedForCommunity(ctx context.Context, communityID string) ([]IndexRow, error) {
	return r.listForCommunity(ctx, communityID, true, nil)
}

// ListVisibleForCommunity is the index-page variant: it hides restricted
// projects that userID may not see (creator/admin/owner + ACL members
// always see; community-visible + open projects are shown to all).
func (r *Repo) ListVisibleForCommunity(ctx context.Context, communityID, userID string, isAdmin, archived bool) ([]IndexRow, error) {
	return r.listForCommunity(ctx, communityID, archived, &viewerFilter{userID: userID, isAdmin: isAdmin})
}

func (r *Repo) listForCommunity(ctx context.Context, communityID string, archived bool, vis *viewerFilter) ([]IndexRow, error) {
	where := "p.archived_at IS NULL"
	order := "p.updated_at DESC"
	if archived {
		where = "p.archived_at IS NOT NULL"
		order = "p.archived_at DESC"
	}
	args := []any{communityID}
	// visClause hides restricted projects the viewer may not see. nil =
	// no filter (internal find-or-create + move pickers). The predicate
	// mirrors EffectiveAccess's read rule in SQL: open project, OR
	// community-visible, OR the viewer is creator/admin, OR the viewer has
	// an ACL row.
	visClause := ""
	if vis != nil {
		if vis.isAdmin {
			// Admin/owner of THIS community sees everything; no predicate.
		} else {
			visClause = ` AND (p.needs_perms = 0
				OR p.visibility = 'community'
				OR p.creator_user_id = ?
				OR EXISTS (SELECT 1 FROM project_members m WHERE m.project_id = p.id AND m.user_id = ?))`
			args = append(args, vis.userID, vis.userID)
		}
	}
	q := fmt.Sprintf(`
		SELECT p.id, p.community_id, p.creator_user_id, p.title,
		       p.description_md, p.description_html, p.archived_at,
		       p.created_at, p.updated_at,
		       p.needs_perms, p.visibility, p.member_access,
		       (SELECT COUNT(*) FROM project_todos t WHERE t.project_id = p.id) AS todo_total,
		       (SELECT COUNT(*) FROM project_todos t WHERE t.project_id = p.id AND t.done = 1) AS todo_done,
		       (SELECT COUNT(*) FROM project_attachments a WHERE a.project_id = p.id) AS att_count,
		       (SELECT COUNT(*) FROM project_comments c WHERE c.project_id = p.id AND c.deleted_at IS NULL) AS cmt_count
		FROM projects p
		WHERE p.community_id = ? AND %s%s
		ORDER BY %s`, where, visClause, order)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var row IndexRow
		var arch sql.NullInt64
		var cAt, uAt int64
		var needsPerms int
		if err := rows.Scan(&row.ID, &row.CommunityID, &row.CreatorUserID, &row.Title,
			&row.DescriptionMD, &row.DescriptionHTML, &arch, &cAt, &uAt,
			&needsPerms, &row.Visibility, &row.MemberAccess,
			&row.TodoTotal, &row.TodoDone, &row.AttachmentCount, &row.CommentCount); err != nil {
			return nil, err
		}
		row.NeedsPerms = needsPerms == 1
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

// viewerFilter scopes a project list to what one caller may see.
type viewerFilter struct {
	userID  string
	isAdmin bool
}

// ByID loads one project. Returns sql.ErrNoRows wrapped if missing.
func (r *Repo) ByID(ctx context.Context, id string) (Project, error) {
	var p Project
	var arch sql.NullInt64
	var cAt, uAt int64
	var needsPerms int
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, creator_user_id, title,
		       description_md, description_html, archived_at, created_at, updated_at,
		       needs_perms, visibility, member_access
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.CommunityID, &p.CreatorUserID, &p.Title,
		&p.DescriptionMD, &p.DescriptionHTML, &arch, &cAt, &uAt,
		&needsPerms, &p.Visibility, &p.MemberAccess)
	if err != nil {
		return Project{}, err
	}
	p.NeedsPerms = needsPerms == 1
	p.CreatedAt = time.UnixMilli(cAt).UTC()
	p.UpdatedAt = time.UnixMilli(uAt).UTC()
	if arch.Valid {
		t := time.UnixMilli(arch.Int64).UTC()
		p.ArchivedAt = &t
	}
	return p, nil
}

// Insert persists a fresh project row, including its permission columns.
func (r *Repo) Insert(ctx context.Context, p Project) error {
	visibility := p.Visibility
	if visibility == "" {
		visibility = VisibilityCommunity
	}
	memberAccess := p.MemberAccess
	if memberAccess == "" {
		memberAccess = AccessRead
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO projects
		  (id, community_id, creator_user_id, title, description_md, description_html,
		   needs_perms, visibility, member_access, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.CommunityID, p.CreatorUserID, p.Title,
		p.DescriptionMD, p.DescriptionHTML,
		boolToInt(p.NeedsPerms), visibility, memberAccess,
		p.CreatedAt.UnixMilli(), p.UpdatedAt.UnixMilli())
	return err
}

// boolToInt maps a Go bool to SQLite's 0/1 integer boolean.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SetPerms updates a project's permission triple and bumps updated_at.
func (r *Repo) SetPerms(ctx context.Context, projectID string, needsPerms bool, visibility, memberAccess string, now time.Time) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE projects SET needs_perms = ?, visibility = ?, member_access = ?, updated_at = ?
		WHERE id = ?`,
		boolToInt(needsPerms), visibility, memberAccess, now.UnixMilli(), projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MemberAccessFor returns the ACL access level granted to userID on
// projectID, and whether a row exists at all.
func (r *Repo) MemberAccessFor(ctx context.Context, projectID, userID string) (string, bool) {
	if userID == "" {
		return "", false
	}
	var access string
	err := r.DB.QueryRowContext(ctx,
		`SELECT access FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID).Scan(&access)
	if err != nil {
		return "", false
	}
	return access, true
}

// ListMembers returns a project's ACL rows with a display-name snapshot
// from the community roster, name-sorted for stable rendering.
func (r *Repo) ListMembers(ctx context.Context, projectID string) ([]ProjectMember, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT pm.project_id, pm.user_id, pm.access, COALESCE(m.display_name, ''), pm.created_at
		FROM project_members pm
		JOIN projects p ON p.id = pm.project_id
		LEFT JOIN memberships m ON m.user_id = pm.user_id AND m.community_id = p.community_id
		WHERE pm.project_id = ?
		ORDER BY m.display_name COLLATE NOCASE, pm.user_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project members: %w", err)
	}
	defer rows.Close()
	var out []ProjectMember
	for rows.Next() {
		var m ProjectMember
		var cAt int64
		if err := rows.Scan(&m.ProjectID, &m.UserID, &m.Access, &m.Name, &cAt); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(cAt).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetProjectMember upserts one ACL grant (insert or change the access).
func (r *Repo) SetProjectMember(ctx context.Context, projectID, userID, access string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_members (project_id, user_id, access, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, user_id) DO UPDATE SET access = excluded.access`,
		projectID, userID, access, now.UnixMilli())
	return err
}

// RemoveProjectMember drops one ACL grant.
func (r *Repo) RemoveProjectMember(ctx context.Context, projectID, userID string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID)
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
// todoSelect is the shared column list + joins for loading a Todo with
// its assignee display-name snapshot. Both ListTodos and TodoByID use it
// so the scan order stays in lockstep (see scanTodo).
const todoSelect = `
	SELECT t.id, t.project_id, t.body, t.done, t.status, t.sort_order, t.created_by,
	       COALESCE(t.assignee_user_id, ''), COALESCE(am.display_name, ''),
	       t.completed_at, t.created_at, t.updated_at
	FROM project_todos t
	JOIN projects p ON p.id = t.project_id
	LEFT JOIN memberships am ON am.user_id = t.assignee_user_id AND am.community_id = p.community_id`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanTodo reads one row in the todoSelect column order.
func scanTodo(s rowScanner) (Todo, error) {
	var t Todo
	var done int
	var completed sql.NullInt64
	var cAt, uAt int64
	if err := s.Scan(&t.ID, &t.ProjectID, &t.Body, &done, &t.Status, &t.SortOrder,
		&t.CreatedBy, &t.AssigneeUserID, &t.AssigneeName, &completed, &cAt, &uAt); err != nil {
		return Todo{}, err
	}
	t.Done = done == 1
	if completed.Valid {
		ct := time.UnixMilli(completed.Int64).UTC()
		t.CompletedAt = &ct
	}
	t.CreatedAt = time.UnixMilli(cAt).UTC()
	t.UpdatedAt = time.UnixMilli(uAt).UTC()
	return t, nil
}

func (r *Repo) ListTodos(ctx context.Context, projectID string) ([]Todo, error) {
	rows, err := r.DB.QueryContext(ctx, todoSelect+`
		WHERE t.project_id = ?
		ORDER BY t.sort_order ASC, t.created_at ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
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
	status := t.Status
	if status == "" {
		status = TodoStatusTodo
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_todos (id, project_id, body, done, status, sort_order, created_by, assignee_user_id, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Body, done, status, t.SortOrder, t.CreatedBy,
		nullStr(t.AssigneeUserID), millisOrNil(t.CompletedAt),
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

// millisOrNil maps a nil time to SQL NULL, else to epoch millis.
func millisOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// UpdateTodoBody changes the text and bumps updated_at.
func (r *Repo) UpdateTodoBody(ctx context.Context, projectID, id, body string, now time.Time) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE project_todos SET body = ?, updated_at = ? WHERE id = ? AND project_id = ?`,
		body, now.UnixMilli(), id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ToggleTodoDone flips between done and todo (the quick checkbox path),
// keeping status + completed_at in sync. The CASE expressions read the
// pre-update `done` value, so entering done stamps completed_at and
// leaving it clears the stamp.
func (r *Repo) ToggleTodoDone(ctx context.Context, projectID, id string, now time.Time) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_todos SET
			done = CASE done WHEN 0 THEN 1 ELSE 0 END,
			status = CASE done WHEN 0 THEN 'done' ELSE 'todo' END,
			completed_at = CASE done WHEN 0 THEN ? ELSE NULL END,
			updated_at = ?
		WHERE id = ? AND project_id = ?`, now.UnixMilli(), now.UnixMilli(), id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTodoStatus sets an explicit status, syncing the done mirror and the
// completion stamp: entering 'done' stamps completed_at, any other status
// clears it.
func (r *Repo) SetTodoStatus(ctx context.Context, projectID, id, status string, now time.Time) error {
	done := 0
	var completed any
	if status == TodoStatusDone {
		done = 1
		completed = now.UnixMilli()
	}
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_todos SET status = ?, done = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND project_id = ?`, status, done, completed, now.UnixMilli(), id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTodoAssignee assigns a member, or unassigns when assigneeUserID is
// empty.
func (r *Repo) SetTodoAssignee(ctx context.Context, projectID, id, assigneeUserID string, now time.Time) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_todos SET assignee_user_id = ?, updated_at = ? WHERE id = ? AND project_id = ?`,
		nullStr(assigneeUserID), now.UnixMilli(), id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTodo removes a row outright.
func (r *Repo) DeleteTodo(ctx context.Context, projectID, id string) error {
	res, err := r.DB.ExecContext(ctx, `DELETE FROM project_todos WHERE id = ? AND project_id = ?`, id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TodoByID loads one row.
func (r *Repo) TodoByID(ctx context.Context, id string) (Todo, error) {
	return scanTodo(r.DB.QueryRowContext(ctx, todoSelect+` WHERE t.id = ?`, id))
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
	// Todos report updated_at (not created_at) so a status toggle or body
	// edit surfaces as fresh activity, not just creation. Discussions
	// contribute both new threads and new replies (replies joined back to
	// their thread to filter by project); deleted rows are excluded.
	rows, err := r.DB.QueryContext(ctx, `
		SELECT * FROM (
			SELECT 'comment'    AS kind, created_at AS at FROM project_comments     WHERE project_id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT 'attachment' AS kind, created_at AS at FROM project_attachments  WHERE project_id = ?
			UNION ALL
			SELECT 'todo'       AS kind, updated_at AS at FROM project_todos        WHERE project_id = ?
			UNION ALL
			SELECT 'discussion' AS kind, created_at AS at FROM project_discussion_threads WHERE project_id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT 'discussion' AS kind, r.created_at AS at
				FROM project_discussion_replies r
				JOIN project_discussion_threads t ON t.id = r.thread_id
				WHERE t.project_id = ? AND r.deleted_at IS NULL AND t.deleted_at IS NULL
			UNION ALL
			SELECT 'project'    AS kind, updated_at AS at FROM projects             WHERE id = ?
		)
		ORDER BY at DESC LIMIT ?`,
		projectID, projectID, projectID, projectID, projectID, projectID, limit)
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
// first. Carries the category column added in migration 00018.
func (r *Repo) ListAttachments(ctx context.Context, projectID string) ([]Attachment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, project_id, upload_id, filename, mime, size_bytes, uploader_id, category, created_at
		FROM project_attachments
		WHERE project_id = ?
		ORDER BY category, created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var cAt int64
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.UploadID, &a.Filename,
			&a.MIME, &a.SizeBytes, &a.UploaderID, &a.Category, &cAt); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(cAt).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAttachment persists one row pointing at an uploads.id.
func (r *Repo) InsertAttachment(ctx context.Context, a Attachment) error {
	cat := a.Category
	if cat == "" {
		cat = "common"
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_attachments
		  (id, project_id, upload_id, filename, mime, size_bytes, uploader_id, category, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectID, a.UploadID, a.Filename, a.MIME, a.SizeBytes,
		a.UploaderID, cat, a.CreatedAt.UnixMilli())
	return err
}

// AttachmentByID loads one attachment row.
func (r *Repo) AttachmentByID(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, upload_id, filename, mime, size_bytes, uploader_id, category, created_at
		FROM project_attachments WHERE id = ?`, id).Scan(
		&a.ID, &a.ProjectID, &a.UploadID, &a.Filename, &a.MIME,
		&a.SizeBytes, &a.UploaderID, &a.Category, &cAt)
	if err != nil {
		return Attachment{}, err
	}
	a.CreatedAt = time.UnixMilli(cAt).UTC()
	return a, nil
}

// MoveAttachmentToProject re-parents one project_attachments row.
// File bytes already live in uploads (SHA-256 deduped); only the
// project_id column moves. The caller must validate target project
// belongs to the same community.
func (r *Repo) MoveAttachmentToProject(ctx context.Context, attID, toProjectID string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_attachments SET project_id = ? WHERE id = ?`,
		toProjectID, attID)
	if err != nil {
		return fmt.Errorf("move attachment project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("attachment %s not found", attID)
	}
	return nil
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
