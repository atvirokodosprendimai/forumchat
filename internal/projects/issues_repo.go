package projects

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListIssues returns a project's issues. statusFilter "" or "all"
// includes every status; any specific status string narrows the query
// to WHERE status = ?. Ordered by updated_at descending.
//
// includeClosed is retained for backwards compatibility — true behaves
// the same as statusFilter="all", false the same as statusFilter="open"
// + triaged + in_progress. Prefer statusFilter on new callers.
func (r *Repo) ListIssues(ctx context.Context, projectID string, includeClosed bool, statusFilter ...string) ([]Issue, error) {
	q := `SELECT id, project_id, title, body_md, body_html, status,
	             COALESCE(creator_user_id,''), COALESCE(creator_guest_id,''),
	             creator_name, created_at, updated_at
	      FROM project_issues
	      WHERE project_id = ?`
	args := []any{projectID}
	switch {
	case len(statusFilter) > 0 && statusFilter[0] != "" && statusFilter[0] != "all":
		q += ` AND status = ?`
		args = append(args, statusFilter[0])
	case !includeClosed:
		q += ` AND status != 'closed'`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := r.DB.QueryContext(ctx, q, args...)
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

// CountIssuesByStatus returns a map of status -> count for one project.
// Used by the issues-tab header to render per-tab badges.
func (r *Repo) CountIssuesByStatus(ctx context.Context, projectID string) (map[string]int, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT status, COUNT(*) FROM project_issues WHERE project_id = ?
		GROUP BY status`, projectID)
	if err != nil {
		return nil, fmt.Errorf("count issues by status: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// CloseAllOpenIssues sets every non-closed issue in a project to
// status=closed. Returns the number of rows touched. Idempotent —
// re-running on an already-closed project is a no-op.
func (r *Repo) CloseAllOpenIssues(ctx context.Context, projectID string, now time.Time) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE project_issues SET status = 'closed', updated_at = ?
		WHERE project_id = ? AND status != 'closed'`,
		now.UnixMilli(), projectID)
	if err != nil {
		return 0, fmt.Errorf("close all issues: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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

// GlobalIssueRow is one row of the cross-community global /issues view.
// Carries enough context (project + community slug) to link back to the
// per-community issue page.
type GlobalIssueRow struct {
	IssueID       string
	Title         string
	Status        string
	UpdatedAt     time.Time
	ProjectID     string
	ProjectTitle  string
	CommunityID   string
	CommunitySlug string
	CommunityName string
}

// RecentIssuesAcrossCommunities returns up to `limit` open issues across
// the given community IDs, newest activity first. Closed issues are
// omitted by default. Used by the global admin /issues page.
func (r *Repo) RecentIssuesAcrossCommunities(ctx context.Context, communityIDs []string, includeClosed bool, limit int) ([]GlobalIssueRow, error) {
	if len(communityIDs) == 0 {
		return nil, nil
	}
	placeholders := ""
	args := make([]any, 0, len(communityIDs)+1)
	for i, cid := range communityIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, cid)
	}
	q := `
		SELECT i.id, i.title, i.status, i.updated_at,
		       p.id, p.title,
		       c.id, c.slug, c.name
		FROM project_issues i
		JOIN projects p     ON p.id = i.project_id
		JOIN communities c  ON c.id = p.community_id
		WHERE p.community_id IN (` + placeholders + `)`
	if !includeClosed {
		q += ` AND i.status != 'closed'`
	}
	q += ` ORDER BY i.updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent issues across communities: %w", err)
	}
	defer rows.Close()
	out := []GlobalIssueRow{}
	for rows.Next() {
		var row GlobalIssueRow
		var uAt int64
		if err := rows.Scan(
			&row.IssueID, &row.Title, &row.Status, &uAt,
			&row.ProjectID, &row.ProjectTitle,
			&row.CommunityID, &row.CommunitySlug, &row.CommunityName,
		); err != nil {
			return nil, err
		}
		row.UpdatedAt = time.UnixMilli(uAt).UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}

// MaxIssueUpdatedAt returns the latest project_issues.updated_at across
// the given communities, as a UnixMilli value. Used by the global
// /issues stream as the baseline for "new since page load". Returns 0
// when no issues exist (safe baseline — any future row will exceed it).
func (r *Repo) MaxIssueUpdatedAt(ctx context.Context, communityIDs []string) (int64, error) {
	if len(communityIDs) == 0 {
		return 0, nil
	}
	placeholders := ""
	args := make([]any, 0, len(communityIDs))
	for i, cid := range communityIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, cid)
	}
	var maxAt sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `
		SELECT MAX(i.updated_at)
		FROM project_issues i
		JOIN projects p ON p.id = i.project_id
		WHERE p.community_id IN (`+placeholders+`)`, args...).Scan(&maxAt)
	if err != nil {
		return 0, fmt.Errorf("max issue updated_at: %w", err)
	}
	if !maxAt.Valid {
		return 0, nil
	}
	return maxAt.Int64, nil
}

// CountIssuesUpdatedAfter returns the number of open project_issues in
// the given communities whose updated_at is strictly greater than the
// baseline. Used by the global /issues stream to drive the "X new —
// refresh" pill.
func (r *Repo) CountIssuesUpdatedAfter(ctx context.Context, communityIDs []string, baselineMilli int64) (int64, error) {
	if len(communityIDs) == 0 {
		return 0, nil
	}
	placeholders := ""
	args := make([]any, 0, len(communityIDs)+1)
	for i, cid := range communityIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, cid)
	}
	args = append(args, baselineMilli)
	var n int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_issues i
		JOIN projects p ON p.id = i.project_id
		WHERE p.community_id IN (`+placeholders+`)
		  AND i.status != 'closed'
		  AND i.updated_at > ?`, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count issues updated after: %w", err)
	}
	return n, nil
}

// IssueBodyRow is the row shape the one-shot decode-bodies CLI walks.
type IssueBodyRow struct {
	ID     string
	BodyMD string
}

// AllIssueBodies streams every project_issues id+body_md. Used by the
// repair pass that fixes issues auto-created from base64-encoded
// email_ingest rows.
func (r *Repo) AllIssueBodies(ctx context.Context) ([]IssueBodyRow, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id, body_md FROM project_issues`)
	if err != nil {
		return nil, fmt.Errorf("list issue bodies: %w", err)
	}
	defer rows.Close()
	out := []IssueBodyRow{}
	for rows.Next() {
		var row IssueBodyRow
		if err := rows.Scan(&row.ID, &row.BodyMD); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
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
