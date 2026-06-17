package chat

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

type Kind string

const (
	KindUser           Kind = "user"
	KindSystem         Kind = "system"
	KindThreadAnnounce Kind = "thread_announce"
)

type Message struct {
	ID               string
	CommunityID      string
	AuthorID         *string
	AuthorName       string
	AuthorAvatar     string
	Kind             Kind
	BodyMarkdown     string
	BodyHTML         string
	RefThreadID      *string
	PromotedThreadID *string // thread that was created from this message via promote-chat
	ReplyToID        *string
	ReplyTo          *ReplyContext
	DeletedAt        *time.Time
	CreatedAt        time.Time
	Attachments      []Attachment
}

// ReplyContext is a denormalised snippet of the message being replied to,
// loaded eagerly via JOIN so the FE can render the quote inline.
type ReplyContext struct {
	ID         string
	AuthorName string
	Snippet    string // plain-text excerpt
}

func (m Message) IsDeleted() bool { return m.DeletedAt != nil }

// Reader is one row in the read-receipt list shown under an own message.
// LastReadAt is the unix-seconds high-water mark this user has acked.
type Reader struct {
	UserID      string
	DisplayName string
	AvatarURL   string
	LastReadAt  time.Time
}

// Attachment is one upload linked to a chat message.
type Attachment struct {
	ID            string
	ChatMessageID string
	UploadID      string
	Position      int
	// Joined from uploads — populated by repo eager-loaders for the
	// render path. Empty on freshly-created rows.
	MIME      string
	Size      int64
	Filename  string
	Kind      string // image|video|audio|pdf|other (derived from MIME)
	CreatedAt time.Time
}

// MIMEKind maps a MIME to one of the render-shape buckets the UI
// uses to decide how to render the attachment in a chat bubble.
func MIMEKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case mime == "application/pdf":
		return "pdf"
	}
	return "other"
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) Insert(ctx context.Context, m Message) error {
	var authorID, refThread, replyTo sql.NullString
	if m.AuthorID != nil {
		authorID = sql.NullString{String: *m.AuthorID, Valid: true}
	}
	if m.RefThreadID != nil {
		refThread = sql.NullString{String: *m.RefThreadID, Valid: true}
	}
	if m.ReplyToID != nil {
		replyTo = sql.NullString{String: *m.ReplyToID, Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_messages (id, community_id, author_id, kind, body_md, body_html, ref_thread_id, reply_to_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, refThread, replyTo, m.CreatedAt.Unix())
	return err
}

func (r *Repo) Recent(ctx context.Context, communityID string, limit int) ([]Message, error) {
	msgs, err := r.listBefore(ctx, communityID, time.Now().Add(48*time.Hour), limit)
	if err != nil {
		return nil, err
	}
	return r.hydrateAttachments(ctx, msgs)
}

func (r *Repo) Before(ctx context.Context, communityID string, before time.Time, limit int) ([]Message, error) {
	msgs, err := r.listBefore(ctx, communityID, before, limit)
	if err != nil {
		return nil, err
	}
	return r.hydrateAttachments(ctx, msgs)
}

// hydrateAttachments runs one batch query to eager-load attachments
// for the given message list, then mutates the Messages in place.
// Empty input → empty output. Errors propagate.
func (r *Repo) hydrateAttachments(ctx context.Context, msgs []Message) ([]Message, error) {
	if len(msgs) == 0 {
		return msgs, nil
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	byMsg, err := r.AttachmentsForMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		msgs[i].Attachments = byMsg[msgs[i].ID]
	}
	return msgs, nil
}

func (r *Repo) listBefore(ctx context.Context, communityID string, before time.Time, limit int) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.community_id, m.author_id, m.kind, m.body_md, m.body_html,
		       m.ref_thread_id, m.promoted_thread_id, m.reply_to_id, m.deleted_at, m.created_at,
		       COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, ''),
		       COALESCE(p.id, ''), COALESCE(pmb.display_name, ''), COALESCE(p.body_md, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		LEFT JOIN chat_messages p ON p.id = m.reply_to_id
		LEFT JOIN memberships pmb ON pmb.user_id = p.author_id AND pmb.community_id = p.community_id
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
		var aid, ref, promoted, reply sql.NullString
		var del sql.NullInt64
		var created int64
		var kind string
		var pID, pAuthor, pBody string
		if err := rows.Scan(&m.ID, &m.CommunityID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
			&ref, &promoted, &reply, &del, &created,
			&m.AuthorName, &m.AuthorAvatar,
			&pID, &pAuthor, &pBody); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		if aid.Valid {
			m.AuthorID = &aid.String
		}
		if ref.Valid {
			m.RefThreadID = &ref.String
		}
		if promoted.Valid {
			m.PromotedThreadID = &promoted.String
		}
		if reply.Valid {
			m.ReplyToID = &reply.String
			if pID != "" {
				m.ReplyTo = &ReplyContext{ID: pID, AuthorName: pAuthor, Snippet: snippet(pBody)}
			}
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

func (r *Repo) ByID(ctx context.Context, id string) (Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.community_id, m.author_id, m.kind, m.body_md, m.body_html,
		       m.ref_thread_id, m.promoted_thread_id, m.reply_to_id, m.deleted_at, m.created_at,
		       COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, ''),
		       COALESCE(p.id, ''), COALESCE(pmb.display_name, ''), COALESCE(p.body_md, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		LEFT JOIN chat_messages p ON p.id = m.reply_to_id
		LEFT JOIN memberships pmb ON pmb.user_id = p.author_id AND pmb.community_id = p.community_id
		WHERE m.id = ?`, id)
	if err != nil {
		return Message{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Message{}, sql.ErrNoRows
	}
	var m Message
	var aid, ref, promoted, reply sql.NullString
	var del sql.NullInt64
	var created int64
	var kind string
	var pID, pAuthor, pBody string
	if err := rows.Scan(&m.ID, &m.CommunityID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
		&ref, &promoted, &reply, &del, &created,
		&m.AuthorName, &m.AuthorAvatar,
		&pID, &pAuthor, &pBody); err != nil {
		return Message{}, err
	}
	m.Kind = Kind(kind)
	if aid.Valid {
		m.AuthorID = &aid.String
	}
	if ref.Valid {
		m.RefThreadID = &ref.String
	}
	if promoted.Valid {
		m.PromotedThreadID = &promoted.String
	}
	if reply.Valid {
		m.ReplyToID = &reply.String
		if pID != "" {
			m.ReplyTo = &ReplyContext{ID: pID, AuthorName: pAuthor, Snippet: snippet(pBody)}
		}
	}
	if del.Valid {
		t := time.Unix(del.Int64, 0)
		m.DeletedAt = &t
	}
	m.CreatedAt = time.Unix(created, 0)
	return m, nil
}

// MarkPromoted records the thread that was created from this chat message.
// Returns false (without error) if the message already has a promoted thread
// — callers should treat that as "someone else got here first".
func (r *Repo) MarkPromoted(ctx context.Context, msgID, threadID string) (bool, error) {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE chat_messages SET promoted_thread_id = ?
		WHERE id = ? AND promoted_thread_id IS NULL`, threadID, msgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ClearPromoted releases the promoted_thread_id link (used when the resulting
// thread is hard-deleted so the chat message can be promoted again).
func (r *Repo) ClearPromoted(ctx context.Context, threadID string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE chat_messages SET promoted_thread_id = NULL WHERE promoted_thread_id = ?`, threadID)
	return err
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

// VerifyUploadsOwned asserts every id in ids is an uploads row owned
// by ownerID + scoped to communityID. Returns nil when every id checks
// out; otherwise the first mismatched/missing id wins. Used by the
// chat send path to defeat a replayed upload-id of someone else's
// file.
func (r *Repo) VerifyUploadsOwned(ctx context.Context, ids []string, ownerID, communityID string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := []any{ownerID, communityID}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `
		SELECT COUNT(*) FROM uploads
		WHERE owner_id = ? AND community_id = ? AND id IN (` + placeholders + `)`
	var have int
	if err := r.DB.QueryRowContext(ctx, q, args...).Scan(&have); err != nil {
		return fmt.Errorf("verify uploads: %w", err)
	}
	if have != len(ids) {
		return errors.New("chat: attachment not owned or in community")
	}
	return nil
}

// InsertWithAttachments persists a message AND its attachment links in
// a single transaction so a partial failure can never leave a message
// with missing attachments (or vice versa). uploadIDs is the ordered
// list of upload row ids to link; position is taken from the slice index.
func (r *Repo) InsertWithAttachments(ctx context.Context, m Message, uploadIDs []string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var authorID, refThread, replyTo sql.NullString
	if m.AuthorID != nil {
		authorID = sql.NullString{String: *m.AuthorID, Valid: true}
	}
	if m.RefThreadID != nil {
		refThread = sql.NullString{String: *m.RefThreadID, Valid: true}
	}
	if m.ReplyToID != nil {
		replyTo = sql.NullString{String: *m.ReplyToID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages (id, community_id, author_id, kind, body_md, body_html, ref_thread_id, reply_to_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, refThread, replyTo, m.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("insert chat_messages: %w", err)
	}
	now := time.Now().Unix()
	for i, uid := range uploadIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_message_attachments (id, chat_message_id, upload_id, position, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			uuid.NewString(), m.ID, uid, i, now); err != nil {
			return fmt.Errorf("insert chat_message_attachments[%d]: %w", i, err)
		}
	}
	return tx.Commit()
}

// AttachmentsForMessages eager-loads every attachment + the joined
// upload row data for a batch of message ids. Returned in
// (message_id, position) order. Empty input → empty output.
func (r *Repo) AttachmentsForMessages(ctx context.Context, msgIDs []string) (map[string][]Attachment, error) {
	if len(msgIDs) == 0 {
		return map[string][]Attachment{}, nil
	}
	placeholders := strings.Repeat("?,", len(msgIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(msgIDs))
	for _, id := range msgIDs {
		args = append(args, id)
	}
	q := `
		SELECT a.id, a.chat_message_id, a.upload_id, a.position, a.created_at,
		       u.mime, u.size, u.filename
		FROM chat_message_attachments a
		JOIN uploads u ON u.id = a.upload_id
		WHERE a.chat_message_id IN (` + placeholders + `)
		ORDER BY a.chat_message_id, a.position`
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]Attachment, len(msgIDs))
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.ChatMessageID, &a.UploadID, &a.Position, &created,
			&a.MIME, &a.Size, &a.Filename); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		a.Kind = MIMEKind(a.MIME)
		out[a.ChatMessageID] = append(out[a.ChatMessageID], a)
	}
	return out, rows.Err()
}

// MarkRead upserts the user's read high-water mark in a community.
// msgID is optional (kept for diagnostics); when empty the row still
// updates the timestamp so the readers query keeps working.
func (r *Repo) MarkRead(ctx context.Context, userID, communityID, msgID string, at time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_reads (user_id, community_id, last_read_at, last_read_msg_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, community_id) DO UPDATE SET
		    last_read_at     = excluded.last_read_at,
		    last_read_msg_id = excluded.last_read_msg_id
	`, userID, communityID, at.Unix(), msgID)
	return err
}

// ReadersSince returns every member of communityID whose last_read_at is
// at least sinceUnix, excluding excludeUserID (typically the sender so
// their own row is not shown as a reader). Ordered most-recent-first.
func (r *Repo) ReadersSince(ctx context.Context, communityID string, sinceUnix int64, excludeUserID string, limit int) ([]Reader, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT r.user_id, COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, ''), r.last_read_at
		FROM chat_reads r
		LEFT JOIN memberships mb ON mb.user_id = r.user_id AND mb.community_id = r.community_id
		WHERE r.community_id = ?
		  AND r.last_read_at >= ?
		  AND r.user_id != ?
		ORDER BY r.last_read_at DESC
		LIMIT ?
	`, communityID, sinceUnix, excludeUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reader
	for rows.Next() {
		var rd Reader
		var at int64
		if err := rows.Scan(&rd.UserID, &rd.DisplayName, &rd.AvatarURL, &at); err != nil {
			return nil, err
		}
		rd.LastReadAt = time.Unix(at, 0)
		out = append(out, rd)
	}
	return out, rows.Err()
}

// snippet returns up to 80 chars of plain-text from a markdown body for
// reply-quote previews.
func snippet(md string) string {
	const max = 80
	if len(md) <= max {
		return md
	}
	return md[:max] + "…"
}

type Service struct {
	Repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

type SendInput struct {
	CommunityID   string
	AuthorID      string
	BodyMarkdown  string
	ReplyToID     *string
	// AttachmentIDs are upload row ids already persisted (e.g. via the
	// chat upload endpoint) that the caller wants linked to this new
	// message. Empty slice → no attachments. The repo verifies each id
	// belongs to AuthorID + CommunityID before linking.
	AttachmentIDs []string
}

func (s *Service) Send(ctx context.Context, in SendInput) (Message, error) {
	body := strings.TrimSpace(in.BodyMarkdown)
	if body == "" && len(in.AttachmentIDs) == 0 {
		return Message{}, errors.New("empty message")
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Message{}, fmt.Errorf("render markdown: %w", err)
	}
	aid := in.AuthorID
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  in.CommunityID,
		AuthorID:     &aid,
		Kind:         KindUser,
		BodyMarkdown: body,
		BodyHTML:     html,
		ReplyToID:    in.ReplyToID,
		CreatedAt:    time.Now(),
	}
	if len(in.AttachmentIDs) == 0 {
		if err := s.Repo.Insert(ctx, m); err != nil {
			return Message{}, fmt.Errorf("insert chat: %w", err)
		}
		return m, nil
	}
	if err := s.Repo.VerifyUploadsOwned(ctx, in.AttachmentIDs, in.AuthorID, in.CommunityID); err != nil {
		return Message{}, err
	}
	if err := s.Repo.InsertWithAttachments(ctx, m, in.AttachmentIDs); err != nil {
		return Message{}, fmt.Errorf("insert chat with atts: %w", err)
	}
	return m, nil
}

// PostSystem inserts a system / thread_announce message (no author).
func (s *Service) PostSystem(ctx context.Context, communityID, bodyHTML string, kind Kind, refThreadID *string) (Message, error) {
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  communityID,
		Kind:         kind,
		BodyMarkdown: bodyHTML, // body_md = body_html for system messages (pre-rendered).
		BodyHTML:     bodyHTML,
		RefThreadID:  refThreadID,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, err
	}
	return m, nil
}
