package projects

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListIssues returns a project's issues. If includeClosed is false,
// closed rows are omitted. Ordered by updated_at descending.
func (r *Repo) ListIssues(ctx context.Context, projectID string, includeClosed bool) ([]Issue, error) {
	q := `SELECT id, project_id, title, body_md, body_html, status,
	             COALESCE(creator_user_id,''), COALESCE(creator_guest_id,''),
	             creator_name, created_at, updated_at
	      FROM project_issues
	      WHERE project_id = ?`
	if !includeClosed {
		q += ` AND status != 'closed'`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := r.DB.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var i Issue
		var cAt, uAt int64
		if err := rows.Scan(&i.ID, &i.ProjectID, &i.Title, &i.BodyMD, &i.BodyHTML,
			&i.Status, &i.CreatorUserID, &i.CreatorGuestID, &i.CreatorName,
			&cAt, &uAt); err != nil {
			return nil, err
		}
		i.CreatedAt = time.UnixMilli(cAt).UTC()
		i.UpdatedAt = time.UnixMilli(uAt).UTC()
		out = append(out, i)
	}
	return out, rows.Err()
}

// IssueByID loads one issue row.
func (r *Repo) IssueByID(ctx context.Context, id string) (Issue, error) {
	var i Issue
	var cAt, uAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, title, body_md, body_html, status,
		       COALESCE(creator_user_id,''), COALESCE(creator_guest_id,''),
		       creator_name, created_at, updated_at
		FROM project_issues WHERE id = ?`, id).Scan(
		&i.ID, &i.ProjectID, &i.Title, &i.BodyMD, &i.BodyHTML, &i.Status,
		&i.CreatorUserID, &i.CreatorGuestID, &i.CreatorName, &cAt, &uAt)
	if err != nil {
		return Issue{}, err
	}
	i.CreatedAt = time.UnixMilli(cAt).UTC()
	i.UpdatedAt = time.UnixMilli(uAt).UTC()
	return i, nil
}

// InsertIssue persists a fresh issue.
func (r *Repo) InsertIssue(ctx context.Context, i Issue) error {
	uid, gid := nullStr(i.CreatorUserID), nullStr(i.CreatorGuestID)
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_issues
		  (id, project_id, title, body_md, body_html, status,
		   creator_user_id, creator_guest_id, creator_name,
		   created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		i.ID, i.ProjectID, i.Title, i.BodyMD, i.BodyHTML, i.Status,
		uid, gid, i.CreatorName,
		i.CreatedAt.UnixMilli(), i.UpdatedAt.UnixMilli())
	return err
}

// UpdateIssueTitle replaces the title and bumps updated_at.
func (r *Repo) UpdateIssueTitle(ctx context.Context, id, title string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_issues SET title = ?, updated_at = ? WHERE id = ?`,
		title, now.UnixMilli(), id)
	return err
}

// UpdateIssueBody persists both markdown and rendered HTML.
func (r *Repo) UpdateIssueBody(ctx context.Context, id, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_issues SET body_md = ?, body_html = ?, updated_at = ? WHERE id = ?`,
		md, html, now.UnixMilli(), id)
	return err
}

// UpdateIssueStatus sets the new status (validated against IssueStatuses).
func (r *Repo) UpdateIssueStatus(ctx context.Context, id, status string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_issues SET status = ?, updated_at = ? WHERE id = ?`,
		status, now.UnixMilli(), id)
	return err
}

// DeleteIssue hard-deletes; FKs cascade comments + attachments.
func (r *Repo) DeleteIssue(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM project_issues WHERE id = ?`, id)
	return err
}

// MoveIssueToProject re-parents one project_issues row. Caller must
// validate target project is in the same community.
func (r *Repo) MoveIssueToProject(ctx context.Context, issueID, toProjectID string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_issues SET project_id = ?, updated_at = ? WHERE id = ?`,
		toProjectID, time.Now().UnixMilli(), issueID)
	if err != nil {
		return fmt.Errorf("move issue project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("issue %s not found", issueID)
	}
	_ = sql.ErrNoRows
	return nil
}

// nullStr returns sql.NullString for empty values so we get NULL in the
// DB instead of the empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CountOpenIssues returns the number of non-closed issues for a project.
func (r *Repo) CountOpenIssues(ctx context.Context, projectID string) (int, error) {
	var n int
	err := r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_issues WHERE project_id = ? AND status != 'closed'`,
		projectID).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return n, nil
}

// ActiveGuestInviteForProject returns the currently-active token for
// a project (one at a time per spec). sql.ErrNoRows if none.
func (r *Repo) ActiveGuestInviteForProject(ctx context.Context, projectID string) (GuestInvite, error) {
	var g GuestInvite
	var exp, rev sql.NullInt64
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT token, project_id, created_by, expires_at, revoked_at, created_at
		FROM project_guest_invites
		WHERE project_id = ? AND revoked_at IS NULL
		ORDER BY created_at DESC LIMIT 1`,
		projectID).Scan(&g.Token, &g.ProjectID, &g.CreatedBy, &exp, &rev, &cAt)
	if err != nil {
		return GuestInvite{}, err
	}
	g.CreatedAt = time.UnixMilli(cAt).UTC()
	if exp.Valid {
		t := time.UnixMilli(exp.Int64).UTC()
		g.ExpiresAt = &t
	}
	return g, nil
}

// GuestInviteByToken loads one invite by token (active or revoked).
func (r *Repo) GuestInviteByToken(ctx context.Context, token string) (GuestInvite, error) {
	var g GuestInvite
	var exp, rev sql.NullInt64
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT token, project_id, created_by, expires_at, revoked_at, created_at
		FROM project_guest_invites WHERE token = ?`,
		token).Scan(&g.Token, &g.ProjectID, &g.CreatedBy, &exp, &rev, &cAt)
	if err != nil {
		return GuestInvite{}, err
	}
	g.CreatedAt = time.UnixMilli(cAt).UTC()
	if exp.Valid {
		t := time.UnixMilli(exp.Int64).UTC()
		g.ExpiresAt = &t
	}
	if rev.Valid {
		t := time.UnixMilli(rev.Int64).UTC()
		g.RevokedAt = &t
	}
	return g, nil
}

// CreateGuestInvite persists a fresh invite row.
func (r *Repo) CreateGuestInvite(ctx context.Context, g GuestInvite) error {
	var exp any
	if g.ExpiresAt != nil {
		exp = g.ExpiresAt.UnixMilli()
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_guest_invites
		  (token, project_id, created_by, expires_at, created_at)
		VALUES (?,?,?,?,?)`,
		g.Token, g.ProjectID, g.CreatedBy, exp, g.CreatedAt.UnixMilli())
	return err
}

// RevokeGuestInvite stamps revoked_at on a token.
func (r *Repo) RevokeGuestInvite(ctx context.Context, token string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_guest_invites SET revoked_at = ? WHERE token = ? AND revoked_at IS NULL`,
		now.UnixMilli(), token)
	return err
}
