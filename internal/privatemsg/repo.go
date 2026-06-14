package privatemsg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("private msg: not found")

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) CreateThread(ctx context.Context, t Thread) error {
	var src any
	if t.SourceCommunityID != "" {
		src = t.SourceCommunityID
	}
	var srcMsg any
	if t.SourceChatMessageID != "" {
		srcMsg = t.SourceChatMessageID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO private_threads
		  (id, initiator_user_id, recipient_user_id, status,
		   source_community_id, source_chat_message_id,
		   last_message_at, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.InitiatorUserID, t.RecipientUserID, string(t.Status),
		src, srcMsg,
		t.LastMessageAt.UnixMilli(), t.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert private_threads: %w", err)
	}
	return nil
}

func (r *Repo) ThreadByID(ctx context.Context, id string) (Thread, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, initiator_user_id, recipient_user_id, status,
		       COALESCE(source_community_id,''), COALESCE(source_chat_message_id,''),
		       last_message_at, created_at
		FROM private_threads WHERE id = ?`, id)
	return scanThread(row)
}

func (r *Repo) ThreadBetween(ctx context.Context, a, b string) (Thread, bool, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, initiator_user_id, recipient_user_id, status,
		       COALESCE(source_community_id,''), COALESCE(source_chat_message_id,''),
		       last_message_at, created_at
		FROM private_threads
		WHERE (initiator_user_id = ? AND recipient_user_id = ?)
		   OR (initiator_user_id = ? AND recipient_user_id = ?)
		ORDER BY last_message_at DESC LIMIT 1`,
		a, b, b, a)
	t, err := scanThread(row)
	if errors.Is(err, ErrNotFound) {
		return Thread{}, false, nil
	}
	if err != nil {
		return Thread{}, false, err
	}
	return t, true, nil
}

func (r *Repo) UpdateThreadStatus(ctx context.Context, id string, s Status) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE private_threads SET status = ? WHERE id = ?`, string(s), id)
	return err
}

func (r *Repo) BumpThreadLastMessage(ctx context.Context, id string, ts time.Time) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE private_threads SET last_message_at = ? WHERE id = ?`, ts.UnixMilli(), id)
	return err
}

func (r *Repo) ListThreadsForUser(ctx context.Context, userID string) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, initiator_user_id, recipient_user_id, status,
		       COALESCE(source_community_id,''), COALESCE(source_chat_message_id,''),
		       last_message_at, created_at
		FROM private_threads
		WHERE initiator_user_id = ? OR recipient_user_id = ?
		ORDER BY last_message_at DESC`,
		userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThreadRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repo) CreateMessage(ctx context.Context, m Message) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO private_messages
		  (id, thread_id, author_user_id, body, body_html, created_at)
		VALUES (?,?,?,?,?,?)`,
		m.ID, m.ThreadID, m.AuthorUserID, m.Body, m.BodyHTML, m.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert private_messages: %w", err)
	}
	return nil
}

func (r *Repo) MessagesByThread(ctx context.Context, threadID string) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, thread_id, author_user_id, body, body_html, created_at
		FROM private_messages
		WHERE thread_id = ?
		ORDER BY created_at ASC`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.AuthorUserID, &m.Body, &m.BodyHTML, &ts); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(ts).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) LatestMessage(ctx context.Context, threadID string) (Message, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, thread_id, author_user_id, body, body_html, created_at
		FROM private_messages WHERE thread_id = ?
		ORDER BY created_at DESC LIMIT 1`, threadID)
	var m Message
	var ts int64
	err := row.Scan(&m.ID, &m.ThreadID, &m.AuthorUserID, &m.Body, &m.BodyHTML, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	m.CreatedAt = time.UnixMilli(ts).UTC()
	return m, nil
}

func (r *Repo) MarkRead(ctx context.Context, threadID, userID string, ts time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO private_thread_reads (thread_id, user_id, last_read_at)
		VALUES (?,?,?)
		ON CONFLICT(thread_id, user_id) DO UPDATE SET last_read_at = excluded.last_read_at`,
		threadID, userID, ts.UnixMilli())
	return err
}

// DisplayName returns the user's display name picked from any of their
// memberships (most recent first). Empty if the user has no membership.
func (r *Repo) DisplayName(ctx context.Context, userID string) (string, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT display_name FROM memberships
		WHERE user_id = ? ORDER BY created_at DESC LIMIT 1`, userID)
	var n string
	err := row.Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return n, nil
}

func (r *Repo) PendingCountForUser(ctx context.Context, userID string) (int, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM private_threads
		WHERE recipient_user_id = ? AND status = 'pending'`, userID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *Repo) UnreadCountForUser(ctx context.Context, userID string) (int, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(uc.cnt), 0) FROM (
		  SELECT t.id,
		    (SELECT COUNT(*) FROM private_messages m
		     WHERE m.thread_id = t.id
		       AND m.author_user_id <> ?
		       AND m.created_at > COALESCE(
		         (SELECT last_read_at FROM private_thread_reads r
		          WHERE r.thread_id = t.id AND r.user_id = ?), 0)) AS cnt
		  FROM private_threads t
		  WHERE (t.initiator_user_id = ? OR t.recipient_user_id = ?)
		    AND t.status = 'accepted'
		) AS uc`,
		userID, userID, userID, userID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// UnreadByThread returns a map threadID -> unread count for the given viewer.
func (r *Repo) UnreadByThread(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.id,
		  (SELECT COUNT(*) FROM private_messages m
		   WHERE m.thread_id = t.id
		     AND m.author_user_id <> ?
		     AND m.created_at > COALESCE(
		       (SELECT last_read_at FROM private_thread_reads r
		        WHERE r.thread_id = t.id AND r.user_id = ?), 0))
		FROM private_threads t
		WHERE t.initiator_user_id = ? OR t.recipient_user_id = ?`,
		userID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanThread(row rowScanner) (Thread, error) {
	var t Thread
	var lastMs, createdMs int64
	var status string
	err := row.Scan(&t.ID, &t.InitiatorUserID, &t.RecipientUserID, &status,
		&t.SourceCommunityID, &t.SourceChatMessageID, &lastMs, &createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	t.Status = Status(status)
	t.LastMessageAt = time.UnixMilli(lastMs).UTC()
	t.CreatedAt = time.UnixMilli(createdMs).UTC()
	return t, nil
}

func scanThreadRows(rows *sql.Rows) (Thread, error) { return scanThread(rows) }
