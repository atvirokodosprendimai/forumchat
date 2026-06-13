package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

type Kind string

const (
	KindUser           Kind = "user"
	KindSystem         Kind = "system"
	KindThreadAnnounce Kind = "thread_announce"
)

type Message struct {
	ID            string
	CommunityID   string
	AuthorID      *string
	AuthorName    string
	AuthorAvatar  string
	Kind          Kind
	BodyMarkdown  string
	BodyHTML      string
	RefThreadID   *string
	DeletedAt     *time.Time
	CreatedAt     time.Time
}

func (m Message) IsDeleted() bool { return m.DeletedAt != nil }

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) Insert(ctx context.Context, m Message) error {
	var authorID sql.NullString
	if m.AuthorID != nil {
		authorID = sql.NullString{String: *m.AuthorID, Valid: true}
	}
	var refThread sql.NullString
	if m.RefThreadID != nil {
		refThread = sql.NullString{String: *m.RefThreadID, Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_messages (id, community_id, author_id, kind, body_md, body_html, ref_thread_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, refThread, m.CreatedAt.Unix())
	return err
}

// Recent returns the last `limit` messages newest-first.
func (r *Repo) Recent(ctx context.Context, communityID string, limit int) ([]Message, error) {
	return r.listBefore(ctx, communityID, time.Now().Add(48*time.Hour), limit)
}

// Before returns messages strictly older than `before`, newest-first.
func (r *Repo) Before(ctx context.Context, communityID string, before time.Time, limit int) ([]Message, error) {
	return r.listBefore(ctx, communityID, before, limit)
}

func (r *Repo) listBefore(ctx context.Context, communityID string, before time.Time, limit int) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.community_id, m.author_id, m.kind, m.body_md, m.body_html, m.ref_thread_id, m.deleted_at, m.created_at,
		       COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		WHERE m.community_id = ? AND m.created_at < ?
		ORDER BY m.created_at DESC
		LIMIT ?`, communityID, before.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var aid, ref sql.NullString
		var del sql.NullInt64
		var created int64
		var kind string
		if err := rows.Scan(&m.ID, &m.CommunityID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML, &ref, &del, &created, &m.AuthorName, &m.AuthorAvatar); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		if aid.Valid {
			m.AuthorID = &aid.String
		}
		if ref.Valid {
			m.RefThreadID = &ref.String
		}
		if del.Valid {
			t := time.Unix(del.Int64, 0)
			m.DeletedAt = &t
		}
		m.CreatedAt = time.Unix(created, 0)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (r *Repo) SoftDelete(ctx context.Context, id string) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE chat_messages SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("chat message not found or already deleted")
	}
	return nil
}

// Service encapsulates chat operations: send (with markdown rendering),
// load history, soft delete.
type Service struct {
	Repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

type SendInput struct {
	CommunityID  string
	AuthorID     string
	BodyMarkdown string
}

func (s *Service) Send(ctx context.Context, in SendInput) (Message, error) {
	if in.BodyMarkdown == "" {
		return Message{}, errors.New("empty message")
	}
	html, err := render.RenderMarkdown(in.BodyMarkdown)
	if err != nil {
		return Message{}, fmt.Errorf("render markdown: %w", err)
	}
	aid := in.AuthorID
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  in.CommunityID,
		AuthorID:     &aid,
		Kind:         KindUser,
		BodyMarkdown: in.BodyMarkdown,
		BodyHTML:     html,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, fmt.Errorf("insert chat: %w", err)
	}
	return m, nil
}

// PostSystem inserts a system / thread_announce message.
func (s *Service) PostSystem(ctx context.Context, communityID, bodyHTML string, kind Kind, refThreadID *string) (Message, error) {
	m := Message{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		Kind:        kind,
		BodyMarkdown: bodyHTML, // for system messages, body_md = body_html (pre-rendered).
		BodyHTML:    bodyHTML,
		RefThreadID: refThreadID,
		CreatedAt:   time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, err
	}
	return m, nil
}
