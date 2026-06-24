// Package notes implements community-wide shared notes ("iNotes"): a member
// writes a note in markdown on a dedicated page, it renders to sanitized HTML
// for reading, and it carries inline comments anchored to the rendered blocks.
//
// A note is 'public' (listed community-wide, member-readable) or 'private' (not
// listed, readable only via an unguessable share_token link or by an editor).
// Editors are the author and any moderator/admin; everyone else reads + comments.
package notes

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// MaxBodyBytes caps a note body. Generous for long documents; defeats a runaway
// note filling the DB.
const MaxBodyBytes = 256 << 10 // 256 KiB

// MaxCommentBytes caps an inline comment.
const MaxCommentBytes = 8 << 10 // 8 KiB

// Visibility values.
const (
	Public  = "public"
	Private = "private"
)

var (
	// ErrEmpty is returned when a save carries no body.
	ErrEmpty = errors.New("notes: empty body")
	// ErrForbidden is returned when a caller may not edit/act on a note.
	ErrForbidden = errors.New("notes: not allowed")
	// ErrNotFound is returned when a note id / token resolves to nothing.
	ErrNotFound = errors.New("notes: not found")
	// ErrBadPatch is returned when a collab sync patch can't be parsed.
	ErrBadPatch = errors.New("notes: malformed patch")
)

// Note is one shared note. Body is the markdown source (shown in the editor);
// BodyHTML is the rendered, sanitized output (shown in the reader). ShareToken
// is the capability that lets a private note be read by link; it is minted on
// first save and kept stable so an already-shared link keeps working.
type Note struct {
	ID          string
	CommunityID string
	ChannelID   *string
	AuthorID    string
	Title       string
	Body        string // published markdown (rendered to BodyHTML, FTS/RAG indexed)
	DraftBody   string // live collaborative draft; published to Body on Save
	BodyHTML    string
	Visibility  string
	ShareToken  string
	Version     int // monotonic; bumped on every merged collab edit and on Save
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IsPublic reports whether the note is listed community-wide.
func (n Note) IsPublic() bool { return n.Visibility == Public }

// CanEdit reports whether id may edit/share/delete the note: the author, a
// moderator/admin of the community, or the platform super-admin. This is the one
// authority for write access — handlers must not re-derive it.
func (n Note) CanEdit(id auth.Identity) bool {
	if id.IsSuperAdmin {
		return true
	}
	if id.User.ID == n.AuthorID {
		return true
	}
	return id.Membership.Role.AtLeast(auth.RoleMod)
}

// Comment is one inline comment anchored to a rendered block of a note.
// BlockIndex is the 0-based position of the top-level rendered block it attaches
// to; Quote is the selected-text snippet for a range comment ("" = a whole-block
// comment). AuthorName is denormalized at read time for display.
type Comment struct {
	ID         string
	NoteID     string
	AuthorID   string
	AuthorName string
	BlockIndex int
	Quote      string
	Body       string
	BodyHTML   string
	ResolvedAt *time.Time
	CreatedAt  time.Time
}

// IsResolved reports whether the comment has been closed.
func (c Comment) IsResolved() bool { return c.ResolvedAt != nil }

// CanModerate reports whether id may resolve/delete the comment: its author, the
// note's author, a moderator/admin, or the super-admin.
func (c Comment) CanModerate(id auth.Identity, note Note) bool {
	if id.User.ID == c.AuthorID {
		return true
	}
	return note.CanEdit(id)
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

const noteCols = `id, community_id, channel_id, author_id, title, body, draft_body, body_html, visibility, share_token, created_at, updated_at, version`

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	var channel sql.NullString
	var created, updated int64
	if err := row.Scan(&n.ID, &n.CommunityID, &channel, &n.AuthorID, &n.Title,
		&n.Body, &n.DraftBody, &n.BodyHTML, &n.Visibility, &n.ShareToken, &created, &updated, &n.Version); err != nil {
		return Note{}, err
	}
	if channel.Valid {
		n.ChannelID = &channel.String
	}
	n.CreatedAt = time.Unix(created, 0)
	n.UpdatedAt = time.Unix(updated, 0)
	return n, nil
}

// Create inserts a note row (typically a fresh empty draft).
func (r *Repo) Create(ctx context.Context, n Note) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO notes (`+noteCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.CommunityID, channelArg(n.ChannelID), n.AuthorID, n.Title,
		n.Body, n.DraftBody, n.BodyHTML, n.Visibility, n.ShareToken, n.CreatedAt.Unix(), n.UpdatedAt.Unix(), n.Version)
	return err
}

// ByID returns a note. Returns sql.ErrNoRows when the id is unknown.
func (r *Repo) ByID(ctx context.Context, id string) (Note, error) {
	return scanNote(r.DB.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes WHERE id = ?`, id))
}

// ByShareToken returns the note carrying token. The token is the bearer
// capability for a private note. Returns sql.ErrNoRows on a miss.
func (r *Repo) ByShareToken(ctx context.Context, token string) (Note, error) {
	return scanNote(r.DB.QueryRowContext(ctx,
		`SELECT `+noteCols+` FROM notes WHERE share_token = ?`, token))
}

// Update persists a note's editable fields plus updated_at, bumping version so
// any open collaborative editors re-sync to the saved body.
func (r *Repo) Update(ctx context.Context, n Note) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE notes SET channel_id = ?, title = ?, body = ?, draft_body = ?, body_html = ?,
			visibility = ?, share_token = ?, updated_at = ?, version = version + 1
		WHERE id = ?`,
		channelArg(n.ChannelID), n.Title, n.Body, n.DraftBody, n.BodyHTML, n.Visibility,
		n.ShareToken, n.UpdatedAt.Unix(), n.ID)
	return err
}

// MergeBody applies a fuzzy diff-match-patch to the note's DRAFT body in one
// transaction (read current → patch → write), bumping version. The single-writer
// DB serializes concurrent merges, so the server is the sequencer and edits from
// several editors converge. The published body / body_html are untouched (Save
// publishes the draft). Returns the merged draft + new version. A malformed
// patch is rejected (ErrBadPatch, no write, no version bump).
func (r *Repo) MergeBody(ctx context.Context, id, patchText string) (string, int, error) {
	dmp := diffmatchpatch.New()
	patches, perr := dmp.PatchFromText(patchText)
	if perr != nil {
		return "", 0, ErrBadPatch
	}
	if len(patches) == 0 {
		// nothing to apply — return current draft+version without a bump.
		var draft string
		var version int
		if err := r.DB.QueryRowContext(ctx, `SELECT draft_body, version FROM notes WHERE id = ?`, id).
			Scan(&draft, &version); err != nil {
			return "", 0, err
		}
		return draft, version, nil
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	var draft string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT draft_body, version FROM notes WHERE id = ?`, id).
		Scan(&draft, &version); err != nil {
		return "", 0, err
	}
	merged, _ := dmp.PatchApply(patches, draft)
	if len(merged) > MaxBodyBytes {
		merged = merged[:MaxBodyBytes]
	}
	version++
	if _, err := tx.ExecContext(ctx,
		`UPDATE notes SET draft_body = ?, version = ?, updated_at = ? WHERE id = ?`,
		merged, version, time.Now().Unix(), id); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return merged, version, nil
}

// Delete removes a note (and its comments, via FK cascade).
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, id)
	return err
}

// ListPublic returns the community's public notes, newest first.
func (r *Repo) ListPublic(ctx context.Context, communityID string, limit int) ([]Note, error) {
	return r.list(ctx, `SELECT `+noteCols+` FROM notes
		WHERE community_id = ? AND visibility = 'public'
		ORDER BY updated_at DESC LIMIT ?`, communityID, limit)
}

// ListByAuthor returns notes authored by authorID in the community (public and
// private), newest first — the author's own list.
func (r *Repo) ListByAuthor(ctx context.Context, communityID, authorID string, limit int) ([]Note, error) {
	return r.list(ctx, `SELECT `+noteCols+` FROM notes
		WHERE community_id = ? AND author_id = ?
		ORDER BY updated_at DESC LIMIT ?`, communityID, authorID, limit)
}

func (r *Repo) list(ctx context.Context, query string, args ...any) ([]Note, error) {
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- comments ---------------------------------------------------------------

// InsertComment persists a new inline comment.
func (r *Repo) InsertComment(ctx context.Context, communityID string, c Comment) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO note_comments (id, note_id, community_id, author_id, block_index, quote, body, body_html, resolved_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		c.ID, c.NoteID, communityID, c.AuthorID, c.BlockIndex, c.Quote, c.Body, c.BodyHTML, c.CreatedAt.Unix())
	return err
}

// CommentByID returns a single comment. Returns sql.ErrNoRows on a miss.
func (r *Repo) CommentByID(ctx context.Context, id string) (Comment, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, note_id, author_id, block_index, quote, body, body_html, resolved_at, created_at
		FROM note_comments WHERE id = ?`, id)
	return scanComment(row)
}

// ListComments returns a note's comments ordered by block then time.
func (r *Repo) ListComments(ctx context.Context, noteID string) ([]Comment, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.note_id, c.author_id, COALESCE(m.effective_display_name, 'member'),
			c.block_index, c.quote, c.body, c.body_html, c.resolved_at, c.created_at
		FROM note_comments c
		JOIN notes n ON n.id = c.note_id
		LEFT JOIN memberships m ON m.user_id = c.author_id AND m.community_id = n.community_id
		WHERE c.note_id = ?
		ORDER BY c.block_index, c.created_at`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		var resolved sql.NullInt64
		var created int64
		if err := rows.Scan(&c.ID, &c.NoteID, &c.AuthorID, &c.AuthorName, &c.BlockIndex,
			&c.Quote, &c.Body, &c.BodyHTML, &resolved, &created); err != nil {
			return nil, err
		}
		if resolved.Valid {
			t := time.Unix(resolved.Int64, 0)
			c.ResolvedAt = &t
		}
		c.CreatedAt = time.Unix(created, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanComment(row interface{ Scan(...any) error }) (Comment, error) {
	var c Comment
	var resolved sql.NullInt64
	var created int64
	if err := row.Scan(&c.ID, &c.NoteID, &c.AuthorID, &c.BlockIndex, &c.Quote,
		&c.Body, &c.BodyHTML, &resolved, &created); err != nil {
		return Comment{}, err
	}
	if resolved.Valid {
		t := time.Unix(resolved.Int64, 0)
		c.ResolvedAt = &t
	}
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
}

// SetCommentResolved stamps/clears a comment's resolved_at.
func (r *Repo) SetCommentResolved(ctx context.Context, id string, at *time.Time) error {
	var ts sql.NullInt64
	if at != nil {
		ts = sql.NullInt64{Int64: at.Unix(), Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE note_comments SET resolved_at = ? WHERE id = ?`, ts, id)
	return err
}

// DeleteComment removes a comment.
func (r *Repo) DeleteComment(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM note_comments WHERE id = ?`, id)
	return err
}

func channelArg(channelID *string) any {
	if channelID == nil {
		return nil
	}
	return *channelID
}

// --- service ----------------------------------------------------------------

type Service struct{ Repo *Repo }

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

// CreateDraft mints an empty private note owned by authorID, opened from
// channelID (pass "" when unknown). The share token is minted up-front so the
// editor's copy/share link is correct before the first save. The caller
// redirects to its editor.
func (s *Service) CreateDraft(ctx context.Context, communityID, channelID, authorID string) (Note, error) {
	now := time.Now()
	tok, err := newToken()
	if err != nil {
		return Note{}, err
	}
	n := Note{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		AuthorID:    authorID,
		Visibility:  Private,
		ShareToken:  tok,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if channelID != "" {
		n.ChannelID = &channelID
	}
	if err := s.Repo.Create(ctx, n); err != nil {
		return Note{}, fmt.Errorf("create note: %w", err)
	}
	return n, nil
}

// SaveInput carries the editor's fields for a note save. Patch is the final
// collab delta to flush before publishing; Body is used only when no patch is
// present (a non-collaborative full save).
type SaveInput struct {
	ID          string
	CommunityID string
	Title       string
	Body        string
	Patch       string
	Visibility  string
}

// Save publishes the note's draft to its rendered `body`/`body_html` (what the
// reader and FTS/RAG see). It enforces community ownership + CanEdit. When a
// collab Patch is present it is merged into the draft FIRST (so Save never
// clobbers a concurrent editor); otherwise a non-empty Body sets the draft
// directly (single-editor save). A share_token is minted on first save.
func (s *Service) Save(ctx context.Context, id auth.Identity, in SaveInput) (Note, error) {
	n, err := s.Repo.ByID(ctx, in.ID)
	if err != nil {
		return Note{}, err
	}
	if n.CommunityID != in.CommunityID {
		return Note{}, ErrForbidden
	}
	if !n.CanEdit(id) {
		return Note{}, ErrForbidden
	}
	if in.Patch != "" {
		// Flush the final delta into the shared draft (merge, not overwrite).
		if _, _, err := s.Repo.MergeBody(ctx, in.ID, in.Patch); err != nil && err != ErrBadPatch {
			return Note{}, fmt.Errorf("flush draft: %w", err)
		}
		n, err = s.Repo.ByID(ctx, in.ID)
		if err != nil {
			return Note{}, err
		}
	} else if strings.TrimSpace(in.Body) != "" {
		n.DraftBody = strings.TrimSpace(in.Body)
	}
	body := strings.TrimSpace(n.DraftBody)
	if body == "" {
		return Note{}, ErrEmpty
	}
	if len(body) > MaxBodyBytes {
		body = body[:MaxBodyBytes]
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Note{}, fmt.Errorf("render note: %w", err)
	}
	n.Title = clip(strings.TrimSpace(in.Title), 160)
	n.Body = body
	n.DraftBody = body // draft == published after a save
	n.BodyHTML = html
	n.Visibility = normalizeVisibility(in.Visibility)
	if n.ShareToken == "" {
		tok, err := newToken()
		if err != nil {
			return Note{}, err
		}
		n.ShareToken = tok
	}
	n.UpdatedAt = time.Now()
	if err := s.Repo.Update(ctx, n); err != nil {
		return Note{}, fmt.Errorf("update note: %w", err)
	}
	return n, nil
}

// SyncBody merges one editor's diff into the note's canonical body (collaborative
// editing). Authorizes (community + CanEdit), then applies the fuzzy patch under
// the sequencing transaction. An empty patch is a no-op cursor-only ping —
// returns the current body+version unchanged. Returns the merged body + version.
func (s *Service) SyncBody(ctx context.Context, id auth.Identity, communityID, noteID, patchText string) (string, int, error) {
	n, err := s.Repo.ByID(ctx, noteID)
	if err != nil {
		return "", 0, err
	}
	if n.CommunityID != communityID {
		return "", 0, ErrForbidden
	}
	if !n.CanEdit(id) {
		return "", 0, ErrForbidden
	}
	if strings.TrimSpace(patchText) == "" {
		return n.DraftBody, n.Version, nil
	}
	return s.Repo.MergeBody(ctx, noteID, patchText)
}

// CommentInput carries a new inline comment.
type CommentInput struct {
	NoteID     string
	BlockIndex int
	Quote      string
	Body       string
}

// AddComment renders and persists an inline comment by id on a note. Any
// approved member may comment; the caller enforces membership. Returns the
// stored comment.
func (s *Service) AddComment(ctx context.Context, communityID string, id auth.Identity, in CommentInput) (Comment, error) {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Comment{}, ErrEmpty
	}
	if len(body) > MaxCommentBytes {
		body = body[:MaxCommentBytes]
	}
	n, err := s.Repo.ByID(ctx, in.NoteID)
	if err != nil {
		return Comment{}, err
	}
	if n.CommunityID != communityID {
		return Comment{}, ErrForbidden
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Comment{}, fmt.Errorf("render comment: %w", err)
	}
	block := in.BlockIndex
	if block < 0 {
		block = 0
	}
	c := Comment{
		ID:         uuid.NewString(),
		NoteID:     in.NoteID,
		AuthorID:   id.User.ID,
		BlockIndex: block,
		Quote:      clip(in.Quote, 280),
		Body:       body,
		BodyHTML:   html,
		CreatedAt:  time.Now(),
	}
	if err := s.Repo.InsertComment(ctx, communityID, c); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	c.AuthorName = id.Membership.DisplayName
	return c, nil
}

// normalizeVisibility coerces an input to a known visibility, defaulting to
// private (the safe default — a note is never made public by accident).
func normalizeVisibility(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == Public {
		return Public
	}
	return Private
}

// newToken mints a 32-byte URL-safe capability token for a private note's link.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("note token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
