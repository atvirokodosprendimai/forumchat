package projects

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListDiscussionThreads returns active threads for a project, most-
// recent activity first. Soft-deleted threads omitted.
func (r *Repo) ListDiscussionThreads(ctx context.Context, projectID string) ([]DiscussionThreadRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.id, t.project_id, t.subject, t.body_md, t.body_html,
		       COALESCE(t.creator_user_id,''), COALESCE(t.creator_guest_id,''),
		       t.creator_name, t.deleted_at, t.last_activity_at, t.created_at, t.updated_at,
		       (SELECT COUNT(*) FROM project_discussion_replies r
		        WHERE r.thread_id = t.id AND r.deleted_at IS NULL) AS reply_count
		FROM project_discussion_threads t
		WHERE t.project_id = ? AND t.deleted_at IS NULL
		ORDER BY t.last_activity_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list discussion threads: %w", err)
	}
	defer rows.Close()
	var out []DiscussionThreadRow
	for rows.Next() {
		var row DiscussionThreadRow
		var del sql.NullInt64
		var la, cAt, uAt int64
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.Subject, &row.BodyMD, &row.BodyHTML,
			&row.CreatorUserID, &row.CreatorGuestID, &row.CreatorName,
			&del, &la, &cAt, &uAt, &row.ReplyCount); err != nil {
			return nil, err
		}
		row.LastActivityAt = time.UnixMilli(la).UTC()
		row.CreatedAt = time.UnixMilli(cAt).UTC()
		row.UpdatedAt = time.UnixMilli(uAt).UTC()
		if del.Valid {
			t := time.UnixMilli(del.Int64).UTC()
			row.DeletedAt = &t
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DiscussionThreadByID loads one thread (incl soft-deleted, caller decides).
func (r *Repo) DiscussionThreadByID(ctx context.Context, id string) (DiscussionThread, error) {
	var t DiscussionThread
	var del sql.NullInt64
	var la, cAt, uAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, project_id, subject, body_md, body_html,
		       COALESCE(creator_user_id,''), COALESCE(creator_guest_id,''),
		       creator_name, deleted_at, last_activity_at, created_at, updated_at
		FROM project_discussion_threads WHERE id = ?`, id).Scan(
		&t.ID, &t.ProjectID, &t.Subject, &t.BodyMD, &t.BodyHTML,
		&t.CreatorUserID, &t.CreatorGuestID, &t.CreatorName,
		&del, &la, &cAt, &uAt)
	if err != nil {
		return DiscussionThread{}, err
	}
	t.LastActivityAt = time.UnixMilli(la).UTC()
	t.CreatedAt = time.UnixMilli(cAt).UTC()
	t.UpdatedAt = time.UnixMilli(uAt).UTC()
	if del.Valid {
		dt := time.UnixMilli(del.Int64).UTC()
		t.DeletedAt = &dt
	}
	return t, nil
}

// InsertDiscussionThread persists a new thread.
func (r *Repo) InsertDiscussionThread(ctx context.Context, t DiscussionThread) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_discussion_threads
		  (id, project_id, subject, body_md, body_html,
		   creator_user_id, creator_guest_id, creator_name,
		   last_activity_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.Subject, t.BodyMD, t.BodyHTML,
		nullStr(t.CreatorUserID), nullStr(t.CreatorGuestID), t.CreatorName,
		t.LastActivityAt.UnixMilli(), t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

// UpdateDiscussionThread replaces subject + body + bumps updated_at.
func (r *Repo) UpdateDiscussionThread(ctx context.Context, id, subject, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_discussion_threads
		 SET subject = ?, body_md = ?, body_html = ?, updated_at = ?
		 WHERE id = ?`,
		subject, md, html, now.UnixMilli(), id)
	return err
}

// BumpDiscussionThreadActivity advances last_activity_at — called when
// a reply is added.
func (r *Repo) BumpDiscussionThreadActivity(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_discussion_threads SET last_activity_at = ? WHERE id = ?`,
		now.UnixMilli(), id)
	return err
}

// SoftDeleteDiscussionThread stamps deleted_at on a thread.
func (r *Repo) SoftDeleteDiscussionThread(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_discussion_threads SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.UnixMilli(), id)
	return err
}

// ListDiscussionReplies returns the chronological reply list.
func (r *Repo) ListDiscussionReplies(ctx context.Context, threadID string) ([]DiscussionReply, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, thread_id, COALESCE(quoted_reply_id,''),
		       COALESCE(author_user_id,''), COALESCE(author_guest_id,''),
		       author_name, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_discussion_replies
		WHERE thread_id = ?
		ORDER BY created_at ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("list discussion replies: %w", err)
	}
	defer rows.Close()
	var out []DiscussionReply
	for rows.Next() {
		var rr DiscussionReply
		var edited, deleted sql.NullInt64
		var cAt int64
		if err := rows.Scan(&rr.ID, &rr.ThreadID, &rr.QuotedReplyID,
			&rr.AuthorUserID, &rr.AuthorGuestID, &rr.AuthorName,
			&rr.BodyMD, &rr.BodyHTML, &edited, &deleted, &cAt); err != nil {
			return nil, err
		}
		rr.CreatedAt = time.UnixMilli(cAt).UTC()
		if edited.Valid {
			t := time.UnixMilli(edited.Int64).UTC()
			rr.EditedAt = &t
		}
		if deleted.Valid {
			t := time.UnixMilli(deleted.Int64).UTC()
			rr.DeletedAt = &t
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// DiscussionReplyByID loads one row.
func (r *Repo) DiscussionReplyByID(ctx context.Context, id string) (DiscussionReply, error) {
	var rr DiscussionReply
	var edited, deleted sql.NullInt64
	var cAt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, thread_id, COALESCE(quoted_reply_id,''),
		       COALESCE(author_user_id,''), COALESCE(author_guest_id,''),
		       author_name, body_md, body_html, edited_at, deleted_at, created_at
		FROM project_discussion_replies WHERE id = ?`, id).Scan(
		&rr.ID, &rr.ThreadID, &rr.QuotedReplyID,
		&rr.AuthorUserID, &rr.AuthorGuestID, &rr.AuthorName,
		&rr.BodyMD, &rr.BodyHTML, &edited, &deleted, &cAt)
	if err != nil {
		return DiscussionReply{}, err
	}
	rr.CreatedAt = time.UnixMilli(cAt).UTC()
	if edited.Valid {
		t := time.UnixMilli(edited.Int64).UTC()
		rr.EditedAt = &t
	}
	if deleted.Valid {
		t := time.UnixMilli(deleted.Int64).UTC()
		rr.DeletedAt = &t
	}
	return rr, nil
}

// InsertDiscussionReply persists a new reply.
func (r *Repo) InsertDiscussionReply(ctx context.Context, rr DiscussionReply) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO project_discussion_replies
		  (id, thread_id, quoted_reply_id, author_user_id, author_guest_id,
		   author_name, body_md, body_html, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rr.ID, rr.ThreadID, nullStr(rr.QuotedReplyID),
		nullStr(rr.AuthorUserID), nullStr(rr.AuthorGuestID),
		rr.AuthorName, rr.BodyMD, rr.BodyHTML, rr.CreatedAt.UnixMilli())
	return err
}

// UpdateDiscussionReply replaces body + bumps edited_at.
func (r *Repo) UpdateDiscussionReply(ctx context.Context, id, md, html string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_discussion_replies SET body_md = ?, body_html = ?, edited_at = ? WHERE id = ?`,
		md, html, now.UnixMilli(), id)
	return err
}

// SoftDeleteDiscussionReply stamps deleted_at.
func (r *Repo) SoftDeleteDiscussionReply(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE project_discussion_replies SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.UnixMilli(), id)
	return err
}
