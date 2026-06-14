package projects

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListIssueComments returns the thread for one issue, ascending.
// Soft-deleted rows are still returned so the handler can decide
// whether to render a tombstone vs hide entirely.
func (r *Repo) ListIssueComments(ctx context.Context, issueID string) ([]IssueComment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, issue_id, COALESCE(author_user_id,''), COALESCE(author_guest_id,''),
		       author_name, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_issue_comments
		WHERE issue_id = ?
		ORDER BY created_at ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue comments: %w", err)
	}
	defer rows.Close()
	var out []IssueComment
	for rows.Next() {
		var c IssueComment
		var edited, deleted sql.NullInt64
		var cAt int64
		if err := rows.Scan(&c.ID, &c.IssueID, &c.AuthorUserID, &c.AuthorGuestID,
			&c.AuthorName, &c.BodyMD, &c.BodyHTML, &edited, &deleted, &cAt); err != nil {
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

// IssueCommentByID loads one row.
func (r *Repo) IssueCommentByID(ctx context.Context, id string) (IssueComment, error) {
	var c IssueComment
	var edited, deleted sql.NullInt64
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, issue_id, COALESCE(author_user_id,''), COALESCE(author_guest_id,''),
		       author_name, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_issue_comments WHERE id = ?`, id).Scan(
		&c.ID, &c.IssueID, &c.AuthorUserID, &c.AuthorGuestID,
		&c.AuthorName, &c.BodyMD, &c.BodyHTML, &edited, &deleted, &cAt)
	if err != nil {
		return IssueComment{}, err
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

// InsertIssueComment persists one new comment.
func (r *Repo) InsertIssueComment(ctx context.Context, c IssueComment) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_issue_comments
		  (id, issue_id, author_user_id, author_guest_id, author_name, body_md, body_html, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		c.ID, c.IssueID, nullStr(c.AuthorUserID), nullStr(c.AuthorGuestID),
		c.AuthorName, c.BodyMD, c.BodyHTML, c.CreatedAt.UnixMilli())
	return err
}

// UpdateIssueComment replaces body + bumps edited_at.
func (r *Repo) UpdateIssueComment(ctx context.Context, id, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_issue_comments SET body_md = ?, body_html = ?, edited_at = ? WHERE id = ?`,
		md, html, now.UnixMilli(), id)
	return err
}

// SoftDeleteIssueComment stamps deleted_at.
func (r *Repo) SoftDeleteIssueComment(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_issue_comments SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.UnixMilli(), id)
	return err
}

// ListIssueAttachments returns image attachments for an issue. When
// commentID is empty we return attachments scoped to the issue body
// itself; otherwise only the attachments on that one comment.
func (r *Repo) ListIssueAttachments(ctx context.Context, issueID string) ([]IssueAttachment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, issue_id, COALESCE(comment_id,''), upload_id,
		       COALESCE(uploader_user_id,''), COALESCE(uploader_guest_id,''),
		       uploader_name, created_at
		FROM project_issue_attachments
		WHERE issue_id = ?
		ORDER BY created_at ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue attachments: %w", err)
	}
	defer rows.Close()
	var out []IssueAttachment
	for rows.Next() {
		var a IssueAttachment
		var cAt int64
		if err := rows.Scan(&a.ID, &a.IssueID, &a.CommentID, &a.UploadID,
			&a.UploaderUserID, &a.UploaderGuestID, &a.UploaderName, &cAt); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(cAt).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertIssueAttachment persists one row.
func (r *Repo) InsertIssueAttachment(ctx context.Context, a IssueAttachment) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_issue_attachments
		  (id, issue_id, comment_id, upload_id, uploader_user_id, uploader_guest_id, uploader_name, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.IssueID, nullStr(a.CommentID), a.UploadID,
		nullStr(a.UploaderUserID), nullStr(a.UploaderGuestID), a.UploaderName,
		a.CreatedAt.UnixMilli())
	return err
}

// IssueAttachmentByID loads one row.
func (r *Repo) IssueAttachmentByID(ctx context.Context, id string) (IssueAttachment, error) {
	var a IssueAttachment
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, issue_id, COALESCE(comment_id,''), upload_id,
		       COALESCE(uploader_user_id,''), COALESCE(uploader_guest_id,''),
		       uploader_name, created_at
		FROM project_issue_attachments WHERE id = ?`, id).Scan(
		&a.ID, &a.IssueID, &a.CommentID, &a.UploadID,
		&a.UploaderUserID, &a.UploaderGuestID, &a.UploaderName, &cAt)
	if err != nil {
		return IssueAttachment{}, err
	}
	a.CreatedAt = time.UnixMilli(cAt).UTC()
	return a, nil
}

// DeleteIssueAttachment removes one row.
func (r *Repo) DeleteIssueAttachment(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM project_issue_attachments WHERE id = ?`, id)
	return err
}
