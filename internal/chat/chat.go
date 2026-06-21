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
	// KindWebhook is an inbound-webhook message. It has no author_id; its
	// display identity (BotName / BotAvatar) is denormalised onto the row
	// so the hot read path needs no JOIN to the webhooks table.
	KindWebhook Kind = "webhook"
	// KindBot is a message posted by a chat-agent (an ai_agents row that
	// participates in the channel). Like KindWebhook it has no author_id and
	// carries a denormalised BotName / BotAvatar, but unlike a webhook it IS a
	// valid @mention target. BotAgentID records which agent posted it and
	// GenStatus tracks its streaming lifecycle.
	KindBot Kind = "bot"
)

type Message struct {
	ID           string
	CommunityID  string
	ChannelID    string
	AuthorID     *string
	AuthorName   string
	AuthorAvatar string
	Kind         Kind
	BodyMarkdown string
	BodyHTML     string
	// BotName / BotAvatar are the display identity of a KindWebhook or
	// KindBot message (empty for every other kind). Denormalised so the chat
	// read path never joins the webhooks / ai_agents tables.
	BotName   string
	BotAvatar string
	// BotAgentID is the ai_agents row that posted a KindBot message; nil for
	// every other kind. GenStatus is the streaming lifecycle of a KindBot
	// bubble ('' | 'generating' | 'done' | 'interrupted'), empty otherwise.
	BotAgentID       *string
	GenStatus        string
	RefThreadID      *string
	PromotedThreadID *string // thread that was created from this message via promote-chat
	ReplyToID        *string
	ReplyTo          *ReplyContext
	// ForwardedFromMsgID points at the original message this one was
	// forwarded from (Discord-style). Cross-channel by design.
	ForwardedFromMsgID *string
	ForwardedFrom      *ForwardContext // eager-loaded source attribution
	DeletedAt          *time.Time
	CreatedAt          time.Time
	Attachments        []Attachment
}

// ReplyContext is a denormalised snippet of the message being replied to,
// loaded eagerly via JOIN so the FE can render the quote inline.
type ReplyContext struct {
	ID         string
	AuthorName string
	Snippet    string // plain-text excerpt
}

// ForwardContext is a denormalised snippet of the source message a
// forward points at — loaded eagerly via JOIN so the bubble can render
// the "Forwarded from #channel" embed without a second query. ChannelSlug
// lets the embed link back to the source channel.
type ForwardContext struct {
	MsgID       string
	ChannelSlug string
	ChannelName string
	AuthorName  string
	Snippet     string // plain-text excerpt of the original body
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

// CreateChannel inserts a channel row. A UNIQUE(community_id, slug)
// collision is mapped to ErrSlugTaken so handlers can render a friendly
// message.
func (r *Repo) CreateChannel(ctx context.Context, c Channel) error {
	var createdBy sql.NullString
	if c.CreatedBy != nil {
		createdBy = sql.NullString{String: *c.CreatedBy, Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_channels (id, community_id, slug, name, topic, position, is_default, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		c.ID, c.CommunityID, c.Slug, c.Name, c.Topic, c.Position, createdBy, c.CreatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrSlugTaken
	}
	return err
}

func (r *Repo) RenameChannel(ctx context.Context, id, name, slug string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE chat_channels SET name = ?, slug = ? WHERE id = ?`, name, slug, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrSlugTaken
	}
	return err
}

func (r *Repo) SetChannelTopic(ctx context.Context, id, topic string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE chat_channels SET topic = ? WHERE id = ?`, topic, id)
	return err
}

func (r *Repo) ArchiveChannel(ctx context.Context, id string, at time.Time) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE chat_channels SET archived_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

func (r *Repo) DeleteChannel(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM chat_channels WHERE id = ?`, id)
	return err
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
	authorID, refThread, replyTo, fwdFrom, botAgentID := m.nullableRefs()
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO chat_messages (id, community_id, channel_id, author_id, kind, body_md, body_html, bot_name, bot_avatar_url, bot_agent_id, gen_status, ref_thread_id, reply_to_id, forwarded_from_msg_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, m.ChannelID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, m.BotName, m.BotAvatar, botAgentID, m.GenStatus, refThread, replyTo, fwdFrom, m.CreatedAt.Unix())
	return err
}

// nullableRefs converts the message's optional foreign-key pointers into
// sql.NullString for the INSERT statements. Shared by Insert and
// InsertWithAttachments so the two write paths can't drift.
func (m Message) nullableRefs() (authorID, refThread, replyTo, fwdFrom, botAgentID sql.NullString) {
	if m.AuthorID != nil {
		authorID = sql.NullString{String: *m.AuthorID, Valid: true}
	}
	if m.RefThreadID != nil {
		refThread = sql.NullString{String: *m.RefThreadID, Valid: true}
	}
	if m.ReplyToID != nil {
		replyTo = sql.NullString{String: *m.ReplyToID, Valid: true}
	}
	if m.ForwardedFromMsgID != nil {
		fwdFrom = sql.NullString{String: *m.ForwardedFromMsgID, Valid: true}
	}
	if m.BotAgentID != nil {
		botAgentID = sql.NullString{String: *m.BotAgentID, Valid: true}
	}
	return
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

// GlobalMessage is one chat row enriched with its community + channel
// identity, for the platform super-admin's cross-community readonly inbox
// (RecentGlobal). It carries only what the inbox renders — no attachments,
// reply context, or forward embed — so the global query stays a single SELECT.
type GlobalMessage struct {
	ID               string
	CommunityID      string
	CommunitySlug    string
	CommunityName    string
	ChannelID        string
	ChannelSlug      string
	ChannelName      string
	AuthorName       string
	Kind             Kind
	BodyHTML         string
	BodyMarkdown     string // plaintext fallback when body_html is blank
	RefThreadID      *string
	PromotedThreadID *string
	Deleted          bool
	CreatedAt        time.Time
}

// RecentGlobal returns the latest `limit` chat messages across EVERY
// community and channel, newest first, for the super-admin's readonly inbox.
// It joins community + channel identity so each row can deep-link back to its
// source. Soft-deleted rows are included (the caller marks them) — this is a
// god-mode view. Single SELECT, no N+1.
func (r *Repo) RecentGlobal(ctx context.Context, limit int) ([]GlobalMessage, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.community_id, c.slug, c.name,
		       COALESCE(m.channel_id, ''), COALESCE(ch.slug, ''), COALESCE(ch.name, ''),
		       COALESCE(mb.display_name, ''), m.kind, m.body_html, m.body_md,
		       m.ref_thread_id, m.promoted_thread_id, m.deleted_at, m.created_at
		FROM chat_messages m
		JOIN communities c ON c.id = m.community_id
		LEFT JOIN chat_channels ch ON ch.id = m.channel_id
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		ORDER BY m.created_at DESC, m.rowid DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GlobalMessage
	for rows.Next() {
		var m GlobalMessage
		var ref, promoted sql.NullString
		var del sql.NullInt64
		var created int64
		var kind string
		if err := rows.Scan(&m.ID, &m.CommunityID, &m.CommunitySlug, &m.CommunityName,
			&m.ChannelID, &m.ChannelSlug, &m.ChannelName,
			&m.AuthorName, &kind, &m.BodyHTML, &m.BodyMarkdown,
			&ref, &promoted, &del, &created); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		if ref.Valid {
			m.RefThreadID = &ref.String
		}
		if promoted.Valid {
			m.PromotedThreadID = &promoted.String
		}
		m.Deleted = del.Valid
		m.CreatedAt = time.Unix(created, 0)
		out = append(out, m)
	}
	return out, rows.Err()
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
		       COALESCE(p.id, ''), COALESCE(pmb.display_name, ''), COALESCE(p.body_md, ''),
		       m.forwarded_from_msg_id,
		       COALESCE(f.id, ''), COALESCE(fch.slug, ''), COALESCE(fch.name, ''), COALESCE(fmb.display_name, ''), COALESCE(f.body_md, ''),
		       COALESCE(m.bot_name, ''), COALESCE(m.bot_avatar_url, ''),
		       m.bot_agent_id, COALESCE(m.gen_status, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		LEFT JOIN chat_messages p ON p.id = m.reply_to_id
		LEFT JOIN memberships pmb ON pmb.user_id = p.author_id AND pmb.community_id = p.community_id
		LEFT JOIN chat_messages f ON f.id = m.forwarded_from_msg_id
		LEFT JOIN chat_channels fch ON fch.id = f.channel_id
		LEFT JOIN memberships fmb ON fmb.user_id = f.author_id AND fmb.community_id = f.community_id
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
		var aid, ref, promoted, reply, fwd sql.NullString
		var del sql.NullInt64
		var created int64
		var kind string
		var pID, pAuthor, pBody string
		var fID, fSlug, fName, fAuthor, fBody string
		var botName, botAvatar, genStatus string
		var botAgentID sql.NullString
		if err := rows.Scan(&m.ID, &m.CommunityID, &m.ChannelID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
			&ref, &promoted, &reply, &del, &created,
			&m.AuthorName, &m.AuthorAvatar,
			&pID, &pAuthor, &pBody,
			&fwd, &fID, &fSlug, &fName, &fAuthor, &fBody,
			&botName, &botAvatar, &botAgentID, &genStatus); err != nil {
			return nil, err
		}
		applyForward(&m, fwd, fID, fSlug, fName, fAuthor, fBody)
		m.Kind = Kind(kind)
		if m.Kind == KindWebhook || m.Kind == KindBot {
			m.BotName, m.BotAvatar = botName, botAvatar
			m.AuthorName, m.AuthorAvatar = botName, botAvatar
			m.GenStatus = genStatus
			if botAgentID.Valid {
				m.BotAgentID = &botAgentID.String
			}
		}
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
		       COALESCE(p.id, ''), COALESCE(pmb.display_name, ''), COALESCE(p.body_md, ''),
		       m.forwarded_from_msg_id,
		       COALESCE(f.id, ''), COALESCE(fch.slug, ''), COALESCE(fch.name, ''), COALESCE(fmb.display_name, ''), COALESCE(f.body_md, ''),
		       COALESCE(m.bot_name, ''), COALESCE(m.bot_avatar_url, ''),
		       m.bot_agent_id, COALESCE(m.gen_status, '')
		FROM chat_messages m
		LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
		LEFT JOIN chat_messages p ON p.id = m.reply_to_id
		LEFT JOIN memberships pmb ON pmb.user_id = p.author_id AND pmb.community_id = p.community_id
		LEFT JOIN chat_messages f ON f.id = m.forwarded_from_msg_id
		LEFT JOIN chat_channels fch ON fch.id = f.channel_id
		LEFT JOIN memberships fmb ON fmb.user_id = f.author_id AND fmb.community_id = f.community_id
		WHERE m.id = ?`, id)
	if err != nil {
		return Message{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Message{}, sql.ErrNoRows
	}
	var m Message
	var aid, ref, promoted, reply, fwd sql.NullString
	var del sql.NullInt64
	var created int64
	var kind string
	var pID, pAuthor, pBody string
	var fID, fSlug, fName, fAuthor, fBody string
	var botName, botAvatar, genStatus string
	var botAgentID sql.NullString
	if err := rows.Scan(&m.ID, &m.CommunityID, &m.ChannelID, &aid, &kind, &m.BodyMarkdown, &m.BodyHTML,
		&ref, &promoted, &reply, &del, &created,
		&m.AuthorName, &m.AuthorAvatar,
		&pID, &pAuthor, &pBody,
		&fwd, &fID, &fSlug, &fName, &fAuthor, &fBody,
		&botName, &botAvatar, &botAgentID, &genStatus); err != nil {
		return Message{}, err
	}
	applyForward(&m, fwd, fID, fSlug, fName, fAuthor, fBody)
	m.Kind = Kind(kind)
	if m.Kind == KindWebhook || m.Kind == KindBot {
		m.BotName, m.BotAvatar = botName, botAvatar
		m.AuthorName, m.AuthorAvatar = botName, botAvatar
		m.GenStatus = genStatus
		if botAgentID.Valid {
			m.BotAgentID = &botAgentID.String
		}
	}
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

	authorID, refThread, replyTo, fwdFrom, botAgentID := m.nullableRefs()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages (id, community_id, channel_id, author_id, kind, body_md, body_html, bot_name, bot_avatar_url, bot_agent_id, gen_status, ref_thread_id, reply_to_id, forwarded_from_msg_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.CommunityID, m.ChannelID, authorID, string(m.Kind), m.BodyMarkdown, m.BodyHTML, m.BotName, m.BotAvatar, botAgentID, m.GenStatus, refThread, replyTo, fwdFrom, m.CreatedAt.Unix()); err != nil {
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

// UploadIDsForMessage returns the upload row ids linked to a chat message,
// in attachment position order. Used by Forward to re-link the source's
// attachments onto the forwarded copy without duplicating files.
func (r *Repo) UploadIDsForMessage(ctx context.Context, msgID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT upload_id FROM chat_message_attachments
		WHERE chat_message_id = ? ORDER BY position`, msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// applyForward populates the forward fields on m from a scanned row. fwd
// is the raw forwarded_from_msg_id; the trailing values are the
// eager-JOINed source message / channel / author and are empty when the
// source row is gone (hard-deleted). Shared by listBefore and ByID so the
// two read paths can't drift.
func applyForward(m *Message, fwd sql.NullString, srcID, chSlug, chName, author, body string) {
	if !fwd.Valid {
		return
	}
	id := fwd.String
	m.ForwardedFromMsgID = &id
	if srcID != "" {
		m.ForwardedFrom = &ForwardContext{
			MsgID:       srcID,
			ChannelSlug: chSlug,
			ChannelName: chName,
			AuthorName:  author,
			Snippet:     snippet(body),
		}
	}
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
	CommunityID  string
	ChannelID    string
	AuthorID     string
	BodyMarkdown string
	ReplyToID    *string
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

// ForwardInput carries a forward of SourceMsgID into TargetChannelID with
// the forwarder's optional Note. AuthorID is the forwarder.
type ForwardInput struct {
	CommunityID     string
	TargetChannelID string
	AuthorID        string
	Note            string
	SourceMsgID     string
}

// Forward creates a message in TargetChannelID carrying a "Forwarded from
// #channel" embed pointing at SourceMsgID, the forwarder's optional Note
// as the body, and re-linked copies of the source's attachments (no file
// copy). The embed itself is content, so an empty note with no attachments
// is still valid. Attachment upload ids are resolved server-side from the
// source — unlike Send, no client-supplied ids reach here, so no ownership
// re-check is needed (the source is already a visible message in the same
// community).
func (s *Service) Forward(ctx context.Context, in ForwardInput) (Message, error) {
	src, err := s.Repo.ByID(ctx, in.SourceMsgID)
	if err != nil {
		return Message{}, fmt.Errorf("load source: %w", err)
	}
	if src.CommunityID != in.CommunityID {
		return Message{}, errors.New("chat: cannot forward across communities")
	}
	uploadIDs, err := s.Repo.UploadIDsForMessage(ctx, src.ID)
	if err != nil {
		return Message{}, fmt.Errorf("load source attachments: %w", err)
	}
	body := strings.TrimSpace(in.Note)
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Message{}, fmt.Errorf("render markdown: %w", err)
	}
	aid := in.AuthorID
	srcID := src.ID
	m := Message{
		ID:                 uuid.NewString(),
		CommunityID:        in.CommunityID,
		ChannelID:          in.TargetChannelID,
		AuthorID:           &aid,
		Kind:               KindUser,
		BodyMarkdown:       body,
		BodyHTML:           html,
		ForwardedFromMsgID: &srcID,
		CreatedAt:          time.Now(),
	}
	if len(uploadIDs) == 0 {
		if err := s.Repo.Insert(ctx, m); err != nil {
			return Message{}, fmt.Errorf("insert forward: %w", err)
		}
		return m, nil
	}
	if err := s.Repo.InsertWithAttachments(ctx, m, uploadIDs); err != nil {
		return Message{}, fmt.Errorf("insert forward with atts: %w", err)
	}
	return m, nil
}

// PostSystem inserts a system / thread_announce message (no author).
// PostSystemMarkdown renders bodyMarkdown and inserts it as a system ("yellow")
// message into a SPECIFIC channel (PostSystem always lands in #general). Used by
// the /summary slash command to post the agent's recap back into the channel it
// was run in, authored by no one.
func (s *Service) PostSystemMarkdown(ctx context.Context, communityID, channelID, bodyMarkdown string) (Message, error) {
	html, err := render.RenderMarkdown(bodyMarkdown)
	if err != nil {
		return Message{}, fmt.Errorf("render markdown: %w", err)
	}
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  communityID,
		ChannelID:    channelID,
		Kind:         KindSystem,
		BodyMarkdown: bodyMarkdown,
		BodyHTML:     html,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, fmt.Errorf("insert system message: %w", err)
	}
	return m, nil
}

// PostBot inserts a KindWebhook message wearing an inbound webhook's bot
// identity (botName / botAvatar) into a specific channel. bodyMarkdown is
// rendered through the standard markdown pipeline; no author_id is set. The
// caller (webhooks.Handler) is responsible for the chat fan-out, same as the
// forum bridge.
func (s *Service) PostBot(ctx context.Context, communityID, channelID, botName, botAvatar, bodyMarkdown string) (Message, error) {
	body := strings.TrimSpace(bodyMarkdown)
	if body == "" {
		return Message{}, errors.New("empty webhook message")
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Message{}, fmt.Errorf("render markdown: %w", err)
	}
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  communityID,
		ChannelID:    channelID,
		Kind:         KindWebhook,
		BotName:      botName,
		BotAvatar:    botAvatar,
		BodyMarkdown: body,
		BodyHTML:     html,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, fmt.Errorf("insert webhook message: %w", err)
	}
	return m, nil
}

// PostSystemHTMLToChannel inserts a system message with pre-rendered, TRUSTED
// bodyHTML into a SPECIFIC channel (PostSystem always lands in #general). The
// caller is responsible for escaping any user-derived text — nothing here runs
// the markdown sanitizer. Used by the /search publish action, whose friendly
// link labels would be stripped by the user-markdown "no hidden URLs" rewrite
// if routed through PostSystemMarkdown.
func (s *Service) PostSystemHTMLToChannel(ctx context.Context, communityID, channelID, bodyHTML string) (Message, error) {
	m := Message{
		ID:           uuid.NewString(),
		CommunityID:  communityID,
		ChannelID:    channelID,
		Kind:         KindSystem,
		BodyMarkdown: bodyHTML, // body_md = body_html for system messages (pre-rendered)
		BodyHTML:     bodyHTML,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Insert(ctx, m); err != nil {
		return Message{}, fmt.Errorf("insert system message: %w", err)
	}
	return m, nil
}

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

// MaxChannelsPerCommunity is the soft cap on non-archived channels. It
// keeps the switcher tidy and the per-channel fan-out cheap; it is a
// product/clarity choice, not a scaling limit.
const MaxChannelsPerCommunity = 10

var (
	ErrEmptyChannelName = errors.New("chat: channel name required")
	ErrReservedSlug     = errors.New("chat: 'general' is reserved")
	ErrChannelCap       = errors.New("chat: channel limit reached")
	ErrSlugTaken        = errors.New("chat: a channel with that name already exists")
	ErrDefaultChannel   = errors.New("chat: the #general channel can't be changed")
)

// slugify lowercases, maps any run of non-alphanumeric chars to a single
// hyphen, and trims leading/trailing hyphens. ASCII-only — enough for
// channel names; non-latin names collapse to empty and are rejected.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// CreateChannel mints a new public channel. Slug is derived from name;
// 'general' is reserved for the default. Enforces the soft cap on
// non-archived channels. createdBy is the acting admin/mod user id.
func (s *Service) CreateChannel(ctx context.Context, communityID, createdBy, name, topic string) (Channel, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Channel{}, ErrEmptyChannelName
	}
	if len(name) > 60 {
		name = name[:60]
	}
	slug := slugify(name)
	if slug == "" {
		return Channel{}, ErrEmptyChannelName
	}
	if slug == "general" {
		return Channel{}, ErrReservedSlug
	}
	existing, err := s.Repo.ListChannels(ctx, communityID, false)
	if err != nil {
		return Channel{}, err
	}
	if len(existing) >= MaxChannelsPerCommunity {
		return Channel{}, ErrChannelCap
	}
	pos := 0
	for _, c := range existing {
		if c.Position >= pos {
			pos = c.Position + 1
		}
	}
	ch := Channel{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		Slug:        slug,
		Name:        name,
		Topic:       strings.TrimSpace(topic),
		Position:    pos,
		CreatedBy:   &createdBy,
		CreatedAt:   time.Now(),
	}
	if err := s.Repo.CreateChannel(ctx, ch); err != nil {
		return Channel{}, err
	}
	return ch, nil
}

// RenameChannel changes a non-default channel's name (and re-derives its
// slug). The default #general can't be renamed.
func (s *Service) RenameChannel(ctx context.Context, communityID, channelID, name string) (Channel, error) {
	ch, err := s.Repo.ChannelByID(ctx, channelID)
	if err != nil {
		return Channel{}, err
	}
	if ch.CommunityID != communityID {
		return Channel{}, sql.ErrNoRows
	}
	if ch.IsDefault {
		return Channel{}, ErrDefaultChannel
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Channel{}, ErrEmptyChannelName
	}
	if len(name) > 60 {
		name = name[:60]
	}
	slug := slugify(name)
	if slug == "" {
		return Channel{}, ErrEmptyChannelName
	}
	if slug == "general" {
		return Channel{}, ErrReservedSlug
	}
	if err := s.Repo.RenameChannel(ctx, channelID, name, slug); err != nil {
		return Channel{}, err
	}
	ch.Name, ch.Slug = name, slug
	return ch, nil
}

// SetTopic updates a channel's topic line. Allowed on any channel
// (including #general).
func (s *Service) SetTopic(ctx context.Context, communityID, channelID, topic string) error {
	ch, err := s.Repo.ChannelByID(ctx, channelID)
	if err != nil {
		return err
	}
	if ch.CommunityID != communityID {
		return sql.ErrNoRows
	}
	if len(topic) > 200 {
		topic = topic[:200]
	}
	return s.Repo.SetChannelTopic(ctx, channelID, strings.TrimSpace(topic))
}

// Archive hides a non-default channel from the switcher and makes it
// read-only. #general can't be archived.
func (s *Service) Archive(ctx context.Context, communityID, channelID string) error {
	ch, err := s.Repo.ChannelByID(ctx, channelID)
	if err != nil {
		return err
	}
	if ch.CommunityID != communityID {
		return sql.ErrNoRows
	}
	if ch.IsDefault {
		return ErrDefaultChannel
	}
	return s.Repo.ArchiveChannel(ctx, channelID, time.Now())
}

// Delete hard-deletes a non-default channel and cascades its messages
// (FK ON DELETE CASCADE). #general can't be deleted.
func (s *Service) Delete(ctx context.Context, communityID, channelID string) error {
	ch, err := s.Repo.ChannelByID(ctx, channelID)
	if err != nil {
		return err
	}
	if ch.CommunityID != communityID {
		return sql.ErrNoRows
	}
	if ch.IsDefault {
		return ErrDefaultChannel
	}
	return s.Repo.DeleteChannel(ctx, channelID)
}
