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
