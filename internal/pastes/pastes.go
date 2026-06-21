// Package pastes implements a per-community pastebin: a member writes a long
// code / markdown / text snippet on a dedicated page instead of flooding a
// chat channel, then posts its URL back into the channel on save.
package pastes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// MaxBodyBytes caps a paste body. Generous for source files; defeats a
// runaway paste filling the DB.
const MaxBodyBytes = 256 << 10 // 256 KiB

var (
	// ErrEmpty is returned when a save carries no body.
	ErrEmpty = errors.New("pastes: empty body")
	// ErrNotDraft is returned when saving a paste that was already posted —
	// pastes are immutable once their link is in the channel.
	ErrNotDraft = errors.New("pastes: already posted")
	// ErrForbidden is returned when a non-author / cross-community caller
	// tries to act on a paste.
	ErrForbidden = errors.New("pastes: not allowed")
)

// Paste is one snippet. PostedAt is nil while the paste is a draft (being
// written); it is stamped when the paste is saved and its URL posted to the
// source channel. ChannelID is the channel the paste was opened from — used
// for the post-back and the return redirect; nil if that channel was deleted.
type Paste struct {
	ID          string
	CommunityID string
	ChannelID   *string
	AuthorID    string
	Title       string
	Language    string
	Body        string
	BodyHTML    string
	PostedAt    *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IsDraft reports whether the paste is still being written (not yet posted).
func (p Paste) IsDraft() bool { return p.PostedAt == nil }

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// Create inserts a paste row (typically a fresh draft).
func (r *Repo) Create(ctx context.Context, p Paste) error {
	var channel sql.NullString
	if p.ChannelID != nil {
		channel = sql.NullString{String: *p.ChannelID, Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO pastes (id, community_id, channel_id, author_id, title, language, body, body_html, posted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		p.ID, p.CommunityID, channel, p.AuthorID, p.Title, p.Language, p.Body, p.BodyHTML,
		p.CreatedAt.Unix(), p.UpdatedAt.Unix())
	return err
}

// ByID returns a paste. Returns sql.ErrNoRows when the id is unknown.
func (r *Repo) ByID(ctx context.Context, id string) (Paste, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, channel_id, author_id, title, language, body, body_html, posted_at, created_at, updated_at
		FROM pastes WHERE id = ?`, id)
	var p Paste
	var channel sql.NullString
	var posted sql.NullInt64
	var created, updated int64
	if err := row.Scan(&p.ID, &p.CommunityID, &channel, &p.AuthorID, &p.Title, &p.Language,
		&p.Body, &p.BodyHTML, &posted, &created, &updated); err != nil {
		return Paste{}, err
	}
	if channel.Valid {
		p.ChannelID = &channel.String
	}
	if posted.Valid {
		t := time.Unix(posted.Int64, 0)
		p.PostedAt = &t
	}
	p.CreatedAt = time.Unix(created, 0)
	p.UpdatedAt = time.Unix(updated, 0)
	return p, nil
}

// Update persists a paste's editable fields plus posted_at / updated_at.
func (r *Repo) Update(ctx context.Context, p Paste) error {
	var posted sql.NullInt64
	if p.PostedAt != nil {
		posted = sql.NullInt64{Int64: p.PostedAt.Unix(), Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		UPDATE pastes SET title = ?, language = ?, body = ?, body_html = ?, posted_at = ?, updated_at = ?
		WHERE id = ?`,
		p.Title, p.Language, p.Body, p.BodyHTML, posted, p.UpdatedAt.Unix(), p.ID)
	return err
}

type Service struct{ Repo *Repo }

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

// CreateDraft mints an empty draft paste owned by authorID, opened from
// channelID (pass "" when unknown). The caller redirects to its page.
func (s *Service) CreateDraft(ctx context.Context, communityID, channelID, authorID string) (Paste, error) {
	now := time.Now()
	p := Paste{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		AuthorID:    authorID,
		Language:    "go",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if channelID != "" {
		p.ChannelID = &channelID
	}
	if err := s.Repo.Create(ctx, p); err != nil {
		return Paste{}, fmt.Errorf("create draft paste: %w", err)
	}
	return p, nil
}

// SaveInput carries the editor's fields for a draft save.
type SaveInput struct {
	ID          string
	CommunityID string
	AuthorID    string
	Title       string
	Language    string
	Body        string
}

// Save renders and persists a draft paste, stamping posted_at. It enforces
// author + community ownership and that the paste is still a draft (immutable
// once posted). Returns the saved paste so the handler can post its URL.
func (s *Service) Save(ctx context.Context, in SaveInput) (Paste, error) {
	p, err := s.Repo.ByID(ctx, in.ID)
	if err != nil {
		return Paste{}, err
	}
	if p.CommunityID != in.CommunityID || p.AuthorID != in.AuthorID {
		return Paste{}, ErrForbidden
	}
	if !p.IsDraft() {
		return Paste{}, ErrNotDraft
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Paste{}, ErrEmpty
	}
	if len(body) > MaxBodyBytes {
		body = body[:MaxBodyBytes]
	}
	html, err := render.RenderMarkdown(renderSource(in.Language, body))
	if err != nil {
		return Paste{}, fmt.Errorf("render paste: %w", err)
	}
	now := time.Now()
	p.Title = strings.TrimSpace(in.Title)
	if len(p.Title) > 120 {
		p.Title = p.Title[:120]
	}
	p.Language = normalizeLanguage(in.Language)
	p.Body = body
	p.BodyHTML = html
	p.PostedAt = &now
	p.UpdatedAt = now
	if err := s.Repo.Update(ctx, p); err != nil {
		return Paste{}, fmt.Errorf("update paste: %w", err)
	}
	return p, nil
}

// renderSource turns a paste body into markdown the shared render pipeline
// understands. "markdown" renders as-is; anything else is wrapped in a fenced
// code block so goldmark emits <pre><code class="language-…"> for styling.
// A fence-safe delimiter (a run of backticks longer than any in the body)
// keeps embedded ``` from breaking out of the block.
func renderSource(language, body string) string {
	lang := normalizeLanguage(language)
	if lang == "markdown" {
		return body
	}
	fence := fenceFor(body)
	tag := lang
	if tag == "text" {
		tag = ""
	}
	return fence + tag + "\n" + body + "\n" + fence
}

// fenceFor returns a backtick run at least 3 long and longer than the longest
// backtick run inside body, so a fenced wrapper can't be closed early.
func fenceFor(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// normalizeLanguage lowercases + trims a language token and defaults blanks to
// plain text. Kept permissive — the value only becomes a CSS class hint.
func normalizeLanguage(language string) string {
	l := strings.ToLower(strings.TrimSpace(language))
	if l == "" {
		return "text"
	}
	return l
}
