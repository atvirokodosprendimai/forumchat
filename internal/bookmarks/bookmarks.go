package bookmarks

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("bookmark: not found")
	ErrEmpty    = errors.New("bookmark: empty")
)

type Bookmark struct {
	ID            string
	UserID        string
	CommunityID   string
	ChatMessageID string
	Title         string
	Category      string
	Note          string
	CreatedAt     time.Time
}

// Row enriches Bookmark with the bookmarked message's author + body for
// display in the list page.
type Row struct {
	Bookmark
	MessageAuthorName string
	MessageSnippet    string
	MessageCreatedAt  time.Time
	MessageDeleted    bool
}

// Filter applies optional list-page filters. Empty fields mean "no filter".
type Filter struct {
	Title    string    // substring match (case-insensitive)
	Category string    // exact match
	From     time.Time // bookmark created at >= From
	To       time.Time // bookmark created at < To
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) Create(ctx context.Context, b Bookmark) (Bookmark, error) {
	if b.ID == "" {
		b.ID = uuid.NewString()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO bookmarks (id, user_id, community_id, chat_message_id, title, category, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.UserID, b.CommunityID, b.ChatMessageID,
		b.Title, b.Category, b.Note, b.CreatedAt.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			// Already bookmarked — surface the existing row.
			existing, gerr := r.byUserMessage(ctx, b.UserID, b.ChatMessageID)
			if gerr == nil {
				return existing, nil
			}
		}
		return Bookmark{}, err
	}
	return b, nil
}

func (r *Repo) byUserMessage(ctx context.Context, userID, msgID string) (Bookmark, error) {
	var b Bookmark
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, community_id, chat_message_id, title, category, note, created_at
		FROM bookmarks WHERE user_id = ? AND chat_message_id = ?`, userID, msgID).
		Scan(&b.ID, &b.UserID, &b.CommunityID, &b.ChatMessageID,
			&b.Title, &b.Category, &b.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Bookmark{}, ErrNotFound
	}
	if err != nil {
		return Bookmark{}, err
	}
	b.CreatedAt = time.Unix(created, 0)
	return b, nil
}

func (r *Repo) ByID(ctx context.Context, id, userID string) (Bookmark, error) {
	var b Bookmark
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, community_id, chat_message_id, title, category, note, created_at
		FROM bookmarks WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&b.ID, &b.UserID, &b.CommunityID, &b.ChatMessageID,
			&b.Title, &b.Category, &b.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Bookmark{}, ErrNotFound
	}
	if err != nil {
		return Bookmark{}, err
	}
	b.CreatedAt = time.Unix(created, 0)
	return b, nil
}

func (r *Repo) Delete(ctx context.Context, id, userID string) error {
	res, err := r.DB.ExecContext(ctx, `DELETE FROM bookmarks WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) Update(ctx context.Context, id, userID, title, category, note string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE bookmarks SET title = ?, category = ?, note = ?
		WHERE id = ? AND user_id = ?`,
		title, category, note, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns the user's bookmarks (JOINed with chat_messages so we have
// author + snippet + original timestamp) ordered newest-bookmarked first.
func (r *Repo) List(ctx context.Context, userID, communityID string, f Filter) ([]Row, error) {
	q := strings.Builder{}
	q.WriteString(`
		SELECT b.id, b.user_id, b.community_id, b.chat_message_id, b.title, b.category, b.note, b.created_at,
		       COALESCE(mb.effective_display_name, ''), COALESCE(cm.body_md, ''), cm.created_at, cm.deleted_at
		FROM bookmarks b
		JOIN chat_messages cm ON cm.id = b.chat_message_id
		LEFT JOIN memberships mb ON mb.user_id = cm.author_id AND mb.community_id = b.community_id
		WHERE b.user_id = ? AND b.community_id = ?`)
	args := []any{userID, communityID}
	if t := strings.TrimSpace(f.Title); t != "" {
		q.WriteString(` AND lower(b.title) LIKE ?`)
		args = append(args, "%"+strings.ToLower(t)+"%")
	}
	if c := strings.TrimSpace(f.Category); c != "" {
		q.WriteString(` AND b.category = ?`)
		args = append(args, c)
	}
	if !f.From.IsZero() {
		q.WriteString(` AND b.created_at >= ?`)
		args = append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		q.WriteString(` AND b.created_at < ?`)
		args = append(args, f.To.Unix())
	}
	q.WriteString(` ORDER BY b.created_at DESC`)
	rows, err := r.DB.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var row Row
		var created, msgCreated int64
		var msgDel sql.NullInt64
		if err := rows.Scan(&row.ID, &row.UserID, &row.CommunityID, &row.ChatMessageID,
			&row.Title, &row.Category, &row.Note, &created,
			&row.MessageAuthorName, &row.MessageSnippet, &msgCreated, &msgDel); err != nil {
			return nil, err
		}
		row.CreatedAt = time.Unix(created, 0)
		row.MessageCreatedAt = time.Unix(msgCreated, 0)
		row.MessageDeleted = msgDel.Valid
		row.MessageSnippet = SnippetForList(row.MessageSnippet)
		out = append(out, row)
	}
	return out, rows.Err()
}

// DistinctCategories returns the user's currently-used categories for a
// filter dropdown.
func (r *Repo) DistinctCategories(ctx context.Context, userID, communityID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT DISTINCT category FROM bookmarks
		WHERE user_id = ? AND community_id = ? AND category != ''
		ORDER BY category ASC`, userID, communityID)
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

// imageMarkdownRE matches a leading markdown image (optionally wrapped in a
// link): `![alt](src)` or `[![alt](src)](href)`.
var imageMarkdownRE = regexp.MustCompile(`^\[?!\[[^\]]*\]\([^)]*\)\]?(?:\([^)]*\))?`)

// stripLeadingImage returns the body with any leading markdown image syntax
// removed and trimmed. If the result is empty, the second return is true so
// callers can substitute a placeholder.
func stripLeadingImage(s string) (string, bool) {
	stripped := strings.TrimSpace(imageMarkdownRE.ReplaceAllString(s, ""))
	return stripped, stripped == "" && strings.TrimSpace(s) != ""
}

// AutoTitleFromMarkdown derives a sensible default title from a message body.
// Image-only messages collapse to "(image)" instead of leaking raw markdown
// link syntax into bookmark titles.
func AutoTitleFromMarkdown(md string) string {
	if i := strings.IndexAny(md, "\r\n"); i >= 0 {
		md = md[:i]
	}
	md = strings.TrimSpace(md)
	stripped, wasImage := stripLeadingImage(md)
	if wasImage {
		return "(image)"
	}
	md = stripped
	if len(md) > 80 {
		md = md[:80] + "…"
	}
	return md
}

// SnippetForList prepares the message-body excerpt shown in the bookmark
// list. Image-only bodies become "(image)" and image-prefixed bodies have
// the image syntax replaced by an "(image) " marker.
func SnippetForList(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	stripped, wasImage := stripLeadingImage(body)
	if wasImage {
		return "(image)"
	}
	if stripped != body {
		body = "(image) " + stripped
	} else {
		body = stripped
	}
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return body
}
