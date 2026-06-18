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
	ChannelID        string
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
	Extracts  []Extract
}

// Extract is one "filed into a project" record. Mode is "docs"
// (project_attachments target) or "issue" (project_issues + issue
// attachment target). ProjectName joined for badge label.
type Extract struct {
	ID                  string
	ChatAttachmentID    string
	ProjectID           string
	ProjectName         string
	ProjectAttachmentID string
	IssueID             string
	Mode                string
	CreatedAt           time.Time
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

// Channel is one named text channel within a community's chat. All
// channels are public — every member reads + writes every non-archived
// channel; there is no membership table. IsDefault marks the
// undeletable #general. ArchivedAt != nil hides it from the switcher and
// makes it read-only.
type Channel struct {
	ID          string
	CommunityID string
	Slug        string
	Name        string
	Topic       string
	Position    int
	IsDefault   bool
	ArchivedAt  *time.Time
	CreatedBy   *string
	CreatedAt   time.Time
}

func (c Channel) IsArchived() bool { return c.ArchivedAt != nil }

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// ListChannels returns a community's channels ordered by position.
// Archived channels are included only when includeArchived is true.
func (r *Repo) ListChannels(ctx context.Context, communityID string, includeArchived bool) ([]Channel, error) {
	q := `
		SELECT id, community_id, slug, name, topic, position, is_default, archived_at, created_by, created_at
		FROM chat_channels
		WHERE community_id = ?`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	q += ` ORDER BY position, created_at`
	rows, err := r.DB.QueryContext(ctx, q, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChannelBySlug resolves one channel within a community. Returns
// sql.ErrNoRows when the slug is unknown.
func (r *Repo) ChannelBySlug(ctx context.Context, communityID, slug string) (Channel, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, slug, name, topic, position, is_default, archived_at, created_by, created_at
		FROM chat_channels WHERE community_id = ? AND slug = ?`, communityID, slug)
	return scanChannel(row)
}

// ChannelByID resolves one channel by primary key.
func (r *Repo) ChannelByID(ctx context.Context, id string) (Channel, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, slug, name, topic, position, is_default, archived_at, created_by, created_at
		FROM chat_channels WHERE id = ?`, id)
	return scanChannel(row)
}

// DefaultChannel returns the community's undeletable #general channel.
func (r *Repo) DefaultChannel(ctx context.Context, communityID string) (Channel, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, slug, name, topic, position, is_default, archived_at, created_by, created_at
		FROM chat_channels WHERE community_id = ? AND is_default = 1`, communityID)
	return scanChannel(row)
}

// EnsureDefaultChannel creates the #general channel for a community if it
// doesn't already have one. Idempotent — safe to call on every boot and
// after creating a new community. Returns the default channel.
func (r *Repo) EnsureDefaultChannel(ctx context.Context, communityID string) (Channel, error) {
	if c, err := r.DefaultChannel(ctx, communityID); err == nil {
		return c, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Channel{}, err
	}
	c := Channel{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		Slug:        "general",
		Name:        "general",
		Position:    0,
		IsDefault:   true,
		CreatedAt:   time.Now(),
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_channels (id, community_id, slug, name, topic, position, is_default, created_by, created_at)
		VALUES (?, ?, ?, ?, '', ?, 1, NULL, ?)`,
		c.ID, c.CommunityID, c.Slug, c.Name, c.Position, c.CreatedAt.Unix()); err != nil {
		return Channel{}, err
	}
	return c, nil
}

// channelScanner is the shared row shape for *sql.Row and *sql.Rows.
type channelScanner interface {
	Scan(dest ...any) error
}

func scanChannel(s channelScanner) (Channel, error) {
	var c Channel
	var topic sql.NullString
	var archived sql.NullInt64
	var createdBy sql.NullString
	var isDefault int
	var created int64
	if err := s.Scan(&c.ID, &c.CommunityID, &c.Slug, &c.Name, &topic, &c.Position,
		&isDefault, &archived, &createdBy, &created); err != nil {
		return Channel{}, err
	}
	c.Topic = topic.String
	c.IsDefault = isDefault == 1
	if archived.Valid {
		t := time.Unix(archived.Int64, 0)
		c.ArchivedAt = &t
	}
	if createdBy.Valid {
		c.CreatedBy = &createdBy.String
	}
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
}

func (r *Repo) Insert(ctx context.Context, m Message) error {
	// Bridge / system writers (forum thread-announce, project digest,
	// room-live) don't pick a channel — those messages belong in
	// #general. Resolve the default channel when ChannelID is empty so
	// those callers don't all need to look it up. The hot user-send path
	// always sets ChannelID explicitly and skips this query.
	if m.ChannelID == "" {
		ch, err := r.DefaultChannel(ctx, m.CommunityID)
		if err != nil {
			return fmt.Errorf("resolve default channel: %w", err)
		}
		m.ChannelID = ch.ID
	}
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
		INSERT INTO chat_messages (id, community_id, channel_id, author_id, kind, body_md, body_html, ref_thread_id, reply_to_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, m.ChannelID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, refThread, replyTo, m.CreatedAt.Unix())
	return err
}

func (r *Repo) Recent(ctx context.Context, channelID string, limit int) ([]Message, error) {
	msgs, err := r.listBefore(ctx, channelID, time.Now().Add(48*time.Hour), limit)
	if err != nil {
		return nil, err
	}
	return r.hydrateAttachments(ctx, msgs)
}

func (r *Repo) Before(ctx context.Context, channelID string, before time.Time, limit int) ([]Message, error) {
	msgs, err := r.listBefore(ctx, channelID, before, limit)
	if err != nil {
		return nil, err
	}
	return r.hydrateAttachments(ctx, msgs)
}

// hydrateAttachments eager-loads attachments AND extracts for the
// given message list — two batch queries total, joined into the
// in-memory tree before the render path needs them.
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
	allAttIDs := make([]string, 0, 16)
	for _, atts := range byMsg {
		for _, a := range atts {
			allAttIDs = append(allAttIDs, a.ID)
		}
	}
	byAtt, err := r.ExtractsForAttachments(ctx, allAttIDs)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		atts := byMsg[msgs[i].ID]
		for j := range atts {
			atts[j].Extracts = byAtt[atts[j].ID]
		}
		msgs[i].Attachments = atts
	}
	return msgs, nil
}

func (r *Repo) listBefore(ctx context.Context, channelID string, before time.Time, limit int) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.community_id, COALESCE(m.channel_id, ''), m.author_id, m.kind, m.body_md, m.body_html,
		       m.ref_thread_id, m.promoted_thread_id, m.reply_to_id, m.deleted_at, m.created_at,
		       COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, ''),
		       COALESCE(p.id, ''), COALESCE(pmb.display_name, ''), COALESCE(p.body_md, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		LEFT JOIN chat_messages p ON p.id = m.reply_to_id
		LEFT JOIN memberships pmb ON pmb.user_id = p.author_id AND pmb.community_id = p.community_id
		WHERE m.channel_id = ? AND m.created_at < ?
		ORDER BY m.created_at DESC
		LIMIT ?`, channelID, before.Unix(), limit)
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
		if err := rows.Scan(&m.ID, &m.CommunityID, &m.ChannelID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
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
		SELECT m.id, m.community_id, COALESCE(m.channel_id, ''), m.author_id, m.kind, m.body_md, m.body_html,
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
	if err := rows.Scan(&m.ID, &m.CommunityID, &m.ChannelID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
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

// AttachmentByID returns a single chat attachment joined with the
// upload metadata it points at. Used by the project-extract path to
// duplicate the upload reference into a project_attachment / issue
// attachment row.
func (r *Repo) AttachmentByID(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT a.id, a.chat_message_id, a.upload_id, a.position, a.created_at,
		       u.mime, u.size, u.filename
		FROM chat_message_attachments a
		JOIN uploads u ON u.id = a.upload_id
		WHERE a.id = ?`, id).
		Scan(&a.ID, &a.ChatMessageID, &a.UploadID, &a.Position, &created,
			&a.MIME, &a.Size, &a.Filename)
	if err != nil {
		return Attachment{}, err
	}
	a.CreatedAt = time.Unix(created, 0)
	a.Kind = MIMEKind(a.MIME)
	return a, nil
}

// InsertExtract records that a chat attachment was filed into a project.
func (r *Repo) InsertExtract(ctx context.Context, e Extract) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_attachment_extracts
		    (id, chat_attachment_id, project_id, project_attachment_id,
		     issue_id, mode, extracted_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ChatAttachmentID, e.ProjectID, e.ProjectAttachmentID,
		e.IssueID, e.Mode, "", e.CreatedAt.Unix())
	return err
}

// ExtractsForAttachments batch-loads every extract row for the given
// chat_message_attachment ids, joined with projects.name for the
// badge label. Returns a map keyed by chat_attachment_id.
func (r *Repo) ExtractsForAttachments(ctx context.Context, attIDs []string) (map[string][]Extract, error) {
	if len(attIDs) == 0 {
		return map[string][]Extract{}, nil
	}
	placeholders := strings.Repeat("?,", len(attIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(attIDs))
	for _, id := range attIDs {
		args = append(args, id)
	}
	q := `
		SELECT e.id, e.chat_attachment_id, e.project_id,
		       COALESCE(p.title, ''),
		       e.project_attachment_id, e.issue_id, e.mode, e.created_at
		FROM chat_attachment_extracts e
		LEFT JOIN projects p ON p.id = e.project_id
		WHERE e.chat_attachment_id IN (` + placeholders + `)
		ORDER BY e.chat_attachment_id, e.created_at DESC`
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]Extract, len(attIDs))
	for rows.Next() {
		var e Extract
		var created int64
		if err := rows.Scan(&e.ID, &e.ChatAttachmentID, &e.ProjectID,
			&e.ProjectName, &e.ProjectAttachmentID, &e.IssueID, &e.Mode, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0)
		out[e.ChatAttachmentID] = append(out[e.ChatAttachmentID], e)
	}
	return out, rows.Err()
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
		INSERT INTO chat_messages (id, community_id, channel_id, author_id, kind, body_md, body_html, ref_thread_id, reply_to_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, m.ChannelID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, refThread, replyTo, m.CreatedAt.Unix()); err != nil {
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

// MarkRead upserts the user's read high-water mark in a channel. The row
// is keyed (user_id, channel_id); community_id is stored for the readers
// query's memberships join. msgID is optional (kept for diagnostics);
// when empty the row still updates the timestamp.
func (r *Repo) MarkRead(ctx context.Context, userID, communityID, channelID, msgID string, at time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_reads (user_id, community_id, channel_id, last_read_at, last_read_msg_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, channel_id) DO UPDATE SET
		    community_id     = excluded.community_id,
		    last_read_at     = excluded.last_read_at,
		    last_read_msg_id = excluded.last_read_msg_id
	`, userID, communityID, channelID, at.Unix(), msgID)
	return err
}

// UnreadChannels returns the set of channel ids in the community that
// have at least one message newer than the viewer's per-channel
// last_read_at. A channel with no chat_reads row for the viewer counts
// as unread when it has any message. Used to seed switcher dots on page
// load. The viewer's own messages don't suppress their channel's dot —
// callers clear the active channel's dot on open regardless.
func (r *Repo) UnreadChannels(ctx context.Context, communityID, userID string) (map[string]bool, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT ch.id
		FROM chat_channels ch
		JOIN chat_messages m ON m.channel_id = ch.id AND m.deleted_at IS NULL
		LEFT JOIN chat_reads rd ON rd.channel_id = ch.id AND rd.user_id = ?
		WHERE ch.community_id = ? AND ch.archived_at IS NULL
		GROUP BY ch.id
		HAVING MAX(m.created_at) > COALESCE(MAX(rd.last_read_at), 0)
	`, userID, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ReadersSince returns every member who has read channelID at or past
// sinceUnix, excluding excludeUserID (typically the sender so their own
// row is not shown as a reader). Ordered most-recent-first.
func (r *Repo) ReadersSince(ctx context.Context, channelID string, sinceUnix int64, excludeUserID string, limit int) ([]Reader, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT r.user_id, COALESCE(mb.display_name, ''), COALESCE(mb.avatar_url, ''), r.last_read_at
		FROM chat_reads r
		LEFT JOIN memberships mb ON mb.user_id = r.user_id AND mb.community_id = r.community_id
		WHERE r.channel_id = ?
		  AND r.last_read_at >= ?
		  AND r.user_id != ?
		ORDER BY r.last_read_at DESC
		LIMIT ?
	`, channelID, sinceUnix, excludeUserID, limit)
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
	ChannelID     string
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
		ChannelID:    in.ChannelID,
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
