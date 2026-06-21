// Package todos implements personal, per-user todo lists scoped to a
// community. Each todo is created from a chat message or a forum post and
// keeps both a body snapshot (so source edits/deletes don't change the
// todo) and a live backlink to the source.
package todos

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusOpen  Status = "open"
	StatusDoing Status = "doing"
	StatusDone  Status = "done"
)

type SourceKind string

const (
	SourceChat      SourceKind = "chat"
	SourceForumPost SourceKind = "forum_post"
	SourceManual    SourceKind = "manual" // standalone, no backlink
)

// Todo is the row a viewer sees on the todos page.
type Todo struct {
	ID             string
	CommunityID    string
	UserID         string
	SourceKind     SourceKind
	SourceID       string
	SourceThreadID string // forum_post only
	SourceDay      string // chat only, YYYY-MM-DD in server-local TZ
	Title          string
	BodySnapshot   string
	Category       string
	Note           string
	Status         Status
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// Filter narrows ListForUser. Empty string means "no filter for that field".
// Status "" or "active" both mean open+doing; "all" means no status filter.
type Filter struct {
	Status   string
	Category string
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

var ErrNotFound = errors.New("todos: not found")

// Create inserts a new todo and returns it. Caller fills SourceKind /
// SourceID / SourceThreadID / SourceDay / Title / BodySnapshot / Category /
// Note; everything else is filled here.
func (r *Repo) Create(ctx context.Context, t Todo) (Todo, error) {
	now := time.Now()
	t.ID = uuid.NewString()
	t.Status = StatusOpen
	t.CreatedAt = now
	t.UpdatedAt = now
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO todos
		(id, community_id, user_id, source_kind, source_id, source_thread_id, source_day,
		 title, body_snapshot, category, note, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.CommunityID, t.UserID, string(t.SourceKind), t.SourceID,
		nullableString(t.SourceThreadID), nullableString(t.SourceDay),
		t.Title, t.BodySnapshot, t.Category, t.Note, string(t.Status),
		now.Unix(), now.Unix(),
	)
	if err != nil {
		return Todo{}, err
	}
	return t, nil
}

// ListForUser returns the viewer's todos in the given community, ordered for
// the page: status ASC (open → doing → done) then created_at DESC.
func (r *Repo) ListForUser(ctx context.Context, userID, communityID string, f Filter) ([]Todo, error) {
	where := []string{"user_id = ?", "community_id = ?"}
	args := []any{userID, communityID}
	switch f.Status {
	case "", "active":
		where = append(where, "status IN ('open','doing')")
	case "open", "doing", "done":
		where = append(where, "status = ?")
		args = append(args, f.Status)
	case "all":
		// no filter
	}
	if c := strings.TrimSpace(f.Category); c != "" {
		where = append(where, "category = ?")
		args = append(args, c)
	}
	query := `
		SELECT id, community_id, user_id, source_kind, source_id,
		       COALESCE(source_thread_id, ''), COALESCE(source_day, ''),
		       title, body_snapshot, category, note, status,
		       created_at, updated_at, completed_at
		FROM todos
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END,
		         created_at DESC`
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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

// ByID returns a single row scoped to this user — callers don't need to
// check ownership separately.
func (r *Repo) ByID(ctx context.Context, userID, todoID string) (Todo, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, user_id, source_kind, source_id,
		       COALESCE(source_thread_id, ''), COALESCE(source_day, ''),
		       title, body_snapshot, category, note, status,
		       created_at, updated_at, completed_at
		FROM todos WHERE id = ? AND user_id = ?`, todoID, userID)
	t, err := scanTodo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	if err != nil {
		return Todo{}, err
	}
	return t, nil
}

// UpdateStatus stamps the new status and bumps updated_at. When transitioning
// into 'done', completed_at is set; when transitioning out, it is cleared.
func (r *Repo) UpdateStatus(ctx context.Context, userID, todoID string, s Status) error {
	now := time.Now().Unix()
	var completed sql.NullInt64
	if s == StatusDone {
		completed = sql.NullInt64{Int64: now, Valid: true}
	}
	res, err := r.DB.ExecContext(ctx, `
		UPDATE todos SET status = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND user_id = ?`, string(s), now, completed, todoID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTitle lets the viewer rename a todo (inline edit on the row).
func (r *Repo) UpdateTitle(ctx context.Context, userID, todoID, title string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE todos SET title = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`, title, time.Now().Unix(), todoID, userID)
	return err
}

// Delete removes the row. Scoped to the viewer.
func (r *Repo) Delete(ctx context.Context, userID, todoID string) error {
	res, err := r.DB.ExecContext(ctx, `DELETE FROM todos WHERE id = ? AND user_id = ?`, todoID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DistinctCategories returns the user's currently-used categories in this
// community for the filter dropdown.
func (r *Repo) DistinctCategories(ctx context.Context, userID, communityID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT DISTINCT category FROM todos
		WHERE user_id = ? AND community_id = ? AND category != ''
		ORDER BY category`, userID, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanTodo(s scannable) (Todo, error) {
	var t Todo
	var kind, status string
	var created, updated int64
	var completed sql.NullInt64
	if err := s.Scan(&t.ID, &t.CommunityID, &t.UserID, &kind, &t.SourceID,
		&t.SourceThreadID, &t.SourceDay,
		&t.Title, &t.BodySnapshot, &t.Category, &t.Note, &status,
		&created, &updated, &completed); err != nil {
		return Todo{}, err
	}
	t.SourceKind = SourceKind(kind)
	t.Status = Status(status)
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	if completed.Valid {
		tt := time.Unix(completed.Int64, 0)
		t.CompletedAt = &tt
	}
	return t, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
