package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

type Thread struct {
	ID             string
	CommunityID    string
	AuthorID       string
	AuthorName     string
	Subject        string
	BodyMarkdown   string
	BodyHTML       string
	DeletedAt      *time.Time
	LastActivityAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (t Thread) IsDeleted() bool { return t.DeletedAt != nil }

type Post struct {
	ID            string
	ThreadID      string
	AuthorID      string
	AuthorName    string
	QuotedPostID  *string
	QuotedBody    string // pre-rendered quote-of-source for inline render
	QuotedAuthor  string
	BodyMarkdown  string
	BodyHTML      string
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (p Post) IsDeleted() bool { return p.DeletedAt != nil }

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// --- threads ---

func (r *Repo) CreateThread(ctx context.Context, t Thread) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO threads (id, community_id, author_id, subject, body_md, body_html, last_activity_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.CommunityID, t.AuthorID, t.Subject, t.BodyMarkdown, t.BodyHTML,
		t.CreatedAt.Unix(), t.CreatedAt.Unix(), t.CreatedAt.Unix())
	return err
}

func (r *Repo) ListThreads(ctx context.Context, communityID string, limit int) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.id, t.community_id, t.author_id, t.subject, t.body_md, t.body_html, t.deleted_at, t.last_activity_at, t.created_at, t.updated_at,
		       COALESCE(mb.display_name, '')
		FROM threads t
		LEFT JOIN memberships mb ON mb.user_id = t.author_id AND mb.community_id = t.community_id
		WHERE t.community_id = ?
		ORDER BY t.last_activity_at DESC
		LIMIT ?`, communityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		var t Thread
		var del sql.NullInt64
		var act, created, updated int64
		if err := rows.Scan(&t.ID, &t.CommunityID, &t.AuthorID, &t.Subject, &t.BodyMarkdown, &t.BodyHTML, &del, &act, &created, &updated, &t.AuthorName); err != nil {
			return nil, err
		}
		if del.Valid {
			tt := time.Unix(del.Int64, 0)
			t.DeletedAt = &tt
		}
		t.LastActivityAt = time.Unix(act, 0)
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repo) GetThread(ctx context.Context, id string) (Thread, error) {
	var t Thread
	var del sql.NullInt64
	var act, created, updated int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT t.id, t.community_id, t.author_id, t.subject, t.body_md, t.body_html, t.deleted_at, t.last_activity_at, t.created_at, t.updated_at,
		       COALESCE(mb.display_name, '')
		FROM threads t
		LEFT JOIN memberships mb ON mb.user_id = t.author_id AND mb.community_id = t.community_id
		WHERE t.id = ?`, id).
		Scan(&t.ID, &t.CommunityID, &t.AuthorID, &t.Subject, &t.BodyMarkdown, &t.BodyHTML, &del, &act, &created, &updated, &t.AuthorName)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	if del.Valid {
		tt := time.Unix(del.Int64, 0)
		t.DeletedAt = &tt
	}
	t.LastActivityAt = time.Unix(act, 0)
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return t, nil
}

func (r *Repo) TouchThread(ctx context.Context, id string, when time.Time) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE threads SET last_activity_at = ?, updated_at = ? WHERE id = ?`,
		when.Unix(), when.Unix(), id)
	return err
}

func (r *Repo) SoftDeleteThread(ctx context.Context, id string) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE threads SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- posts ---

func (r *Repo) CreatePost(ctx context.Context, p Post) error {
	var quoted sql.NullString
	if p.QuotedPostID != nil {
		quoted = sql.NullString{String: *p.QuotedPostID, Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO posts (id, thread_id, author_id, quoted_post_id, body_md, body_html, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ThreadID, p.AuthorID, quoted, p.BodyMarkdown, p.BodyHTML, p.CreatedAt.Unix(), p.CreatedAt.Unix())
	return err
}

func (r *Repo) ListPosts(ctx context.Context, threadID string) ([]Post, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT p.id, p.thread_id, p.author_id, p.quoted_post_id, p.body_md, p.body_html, p.deleted_at, p.created_at, p.updated_at,
		       COALESCE(mb.display_name, ''),
		       COALESCE(qp.body_html, ''), COALESCE(qmb.display_name, '')
		FROM posts p
		LEFT JOIN threads th ON th.id = p.thread_id
		LEFT JOIN memberships mb ON mb.user_id = p.author_id AND mb.community_id = th.community_id
		LEFT JOIN posts qp ON qp.id = p.quoted_post_id
		LEFT JOIN memberships qmb ON qmb.user_id = qp.author_id AND qmb.community_id = th.community_id
		WHERE p.thread_id = ?
		ORDER BY p.created_at ASC`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		var p Post
		var quoted sql.NullString
		var del sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&p.ID, &p.ThreadID, &p.AuthorID, &quoted, &p.BodyMarkdown, &p.BodyHTML, &del, &created, &updated, &p.AuthorName, &p.QuotedBody, &p.QuotedAuthor); err != nil {
			return nil, err
		}
		if quoted.Valid {
			p.QuotedPostID = &quoted.String
		}
		if del.Valid {
			t := time.Unix(del.Int64, 0)
			p.DeletedAt = &t
		}
		p.CreatedAt = time.Unix(created, 0)
		p.UpdatedAt = time.Unix(updated, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetPost(ctx context.Context, id string) (Post, error) {
	var p Post
	var quoted sql.NullString
	var del sql.NullInt64
	var created, updated int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, thread_id, author_id, quoted_post_id, body_md, body_html, deleted_at, created_at, updated_at
		FROM posts WHERE id = ?`, id).
		Scan(&p.ID, &p.ThreadID, &p.AuthorID, &quoted, &p.BodyMarkdown, &p.BodyHTML, &del, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	if err != nil {
		return Post{}, err
	}
	if quoted.Valid {
		p.QuotedPostID = &quoted.String
	}
	if del.Valid {
		t := time.Unix(del.Int64, 0)
		p.DeletedAt = &t
	}
	p.CreatedAt = time.Unix(created, 0)
	p.UpdatedAt = time.Unix(updated, 0)
	return p, nil
}

func (r *Repo) UpdatePost(ctx context.Context, id, md, html string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE posts SET body_md = ?, body_html = ?, updated_at = ? WHERE id = ?`,
		md, html, time.Now().Unix(), id)
	return err
}

func (r *Repo) SoftDeletePost(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE posts SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), id)
	return err
}

// --- service ---

var ErrNotFound = errors.New("forum: not found")
var ErrEmpty = errors.New("forum: empty content")

type Service struct {
	Repo      *Repo
	EditGrace time.Duration
}

func NewService(repo *Repo, grace time.Duration) *Service { return &Service{Repo: repo, EditGrace: grace} }

type CreateThreadInput struct {
	CommunityID  string
	AuthorID     string
	Subject      string
	BodyMarkdown string
}

func (s *Service) CreateThread(ctx context.Context, in CreateThreadInput) (Thread, error) {
	if in.Subject == "" || in.BodyMarkdown == "" {
		return Thread{}, ErrEmpty
	}
	html, err := render.RenderMarkdown(in.BodyMarkdown)
	if err != nil {
		return Thread{}, fmt.Errorf("render markdown: %w", err)
	}
	now := time.Now()
	t := Thread{
		ID:             uuid.NewString(),
		CommunityID:    in.CommunityID,
		AuthorID:       in.AuthorID,
		Subject:        in.Subject,
		BodyMarkdown:   in.BodyMarkdown,
		BodyHTML:       html,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.Repo.CreateThread(ctx, t); err != nil {
		return Thread{}, err
	}
	return t, nil
}

type CreatePostInput struct {
	ThreadID     string
	AuthorID     string
	QuotedPostID *string
	BodyMarkdown string
}

func (s *Service) CreatePost(ctx context.Context, in CreatePostInput) (Post, error) {
	if in.BodyMarkdown == "" {
		return Post{}, ErrEmpty
	}
	html, err := render.RenderMarkdown(in.BodyMarkdown)
	if err != nil {
		return Post{}, err
	}
	now := time.Now()
	p := Post{
		ID:           uuid.NewString(),
		ThreadID:     in.ThreadID,
		AuthorID:     in.AuthorID,
		QuotedPostID: in.QuotedPostID,
		BodyMarkdown: in.BodyMarkdown,
		BodyHTML:     html,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.Repo.CreatePost(ctx, p); err != nil {
		return Post{}, err
	}
	if err := s.Repo.TouchThread(ctx, in.ThreadID, now); err != nil {
		return Post{}, err
	}
	return p, nil
}

func (s *Service) CanEditOrDeleteOwn(p Post, now time.Time) bool {
	return now.Sub(p.CreatedAt) <= s.EditGrace
}
