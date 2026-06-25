package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

type Thread struct {
	ID          string
	CommunityID string
	AuthorID    string
	AuthorName  string
	// AgentID, when set, marks this as an agent-owned thread: every member
	// reply is a prompt and the agent answers as the next post.
	AgentID        *string
	Subject        string
	BodyMarkdown   string
	BodyHTML       string
	DeletedAt      *time.Time
	ResolvedAt     *time.Time
	ResolvedBy     *string
	LastActivityAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (t Thread) IsDeleted() bool  { return t.DeletedAt != nil }
func (t Thread) IsResolved() bool { return t.ResolvedAt != nil }

type Post struct {
	ID           string
	ThreadID     string
	AuthorID     string
	AuthorName   string
	AuthorAvatar string
	QuotedPostID *string
	QuotedBody   string // pre-rendered quote-of-source for inline render
	QuotedAuthor string
	BodyMarkdown string
	BodyHTML     string
	// AgentID set → this post is an agent reply. BotName/BotAvatar are its
	// denormalised display identity; GenStatus is the streaming lifecycle
	// ('' | generating | done | interrupted).
	AgentID   *string
	BotName   string
	BotAvatar string
	GenStatus string
	// ToolCalls is the JSON-encoded tool trace of an agent reply (the agentic
	// loop's internal-search / MCP calls); empty for everything else.
	ToolCalls string
	DeletedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsBot reports whether this post is an agent reply.
func (p Post) IsBot() bool { return p.AgentID != nil }

func (p Post) IsDeleted() bool { return p.DeletedAt != nil }

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// --- threads ---

func (r *Repo) CreateThread(ctx context.Context, t Thread) error {
	var agentID any
	if t.AgentID != nil {
		agentID = *t.AgentID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO threads (id, community_id, author_id, subject, body_md, body_html, agent_id, last_activity_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.CommunityID, t.AuthorID, t.Subject, t.BodyMarkdown, t.BodyHTML, agentID,
		t.CreatedAt.Unix(), t.CreatedAt.Unix(), t.CreatedAt.Unix())
	return err
}

// ListThreadsFiltered returns threads matching the optional status and
// case-insensitive subject/body substring search. status is "", "resolved",
// or "unresolved"; any other value behaves as "".
func (r *Repo) ListThreadsFiltered(ctx context.Context, communityID, status, q string, limit int) ([]Thread, error) {
	args := []any{communityID}
	where := []string{"t.community_id = ?", "t.deleted_at IS NULL"}
	switch status {
	case "resolved":
		where = append(where, "t.resolved_at IS NOT NULL")
	case "unresolved":
		where = append(where, "t.resolved_at IS NULL")
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		where = append(where, "(LOWER(t.subject) LIKE ? OR LOWER(t.body_md) LIKE ?)")
		args = append(args, like, like)
	}
	args = append(args, limit)
	query := `
		SELECT t.id, t.community_id, t.author_id, t.subject, t.body_md, t.body_html, t.deleted_at, t.resolved_at, t.resolved_by, t.last_activity_at, t.created_at, t.updated_at,
		       COALESCE(mb.effective_display_name, ''), t.agent_id
		FROM threads t
		LEFT JOIN memberships mb ON mb.user_id = t.author_id AND mb.community_id = t.community_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY t.last_activity_at DESC
		LIMIT ?`
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repo) ListThreads(ctx context.Context, communityID string, limit int) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.id, t.community_id, t.author_id, t.subject, t.body_md, t.body_html, t.deleted_at, t.resolved_at, t.resolved_by, t.last_activity_at, t.created_at, t.updated_at,
		       COALESCE(mb.effective_display_name, ''), t.agent_id
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
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanThread(s scannable) (Thread, error) {
	var t Thread
	var del, res sql.NullInt64
	var resBy, agentID sql.NullString
	var act, created, updated int64
	if err := s.Scan(&t.ID, &t.CommunityID, &t.AuthorID, &t.Subject, &t.BodyMarkdown, &t.BodyHTML, &del, &res, &resBy, &act, &created, &updated, &t.AuthorName, &agentID); err != nil {
		return Thread{}, err
	}
	if agentID.Valid {
		t.AgentID = &agentID.String
	}
	if del.Valid {
		tt := time.Unix(del.Int64, 0)
		t.DeletedAt = &tt
	}
	if res.Valid {
		tt := time.Unix(res.Int64, 0)
		t.ResolvedAt = &tt
	}
	if resBy.Valid {
		s := resBy.String
		t.ResolvedBy = &s
	}
	t.LastActivityAt = time.Unix(act, 0)
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return t, nil
}

func (r *Repo) GetThread(ctx context.Context, id string) (Thread, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT t.id, t.community_id, t.author_id, t.subject, t.body_md, t.body_html, t.deleted_at, t.resolved_at, t.resolved_by, t.last_activity_at, t.created_at, t.updated_at,
		       COALESCE(mb.effective_display_name, ''), t.agent_id
		FROM threads t
		LEFT JOIN memberships mb ON mb.user_id = t.author_id AND mb.community_id = t.community_id
		WHERE t.id = ?`, id)
	t, err := scanThread(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	return t, nil
}

func (r *Repo) MarkResolved(ctx context.Context, threadID, byUserID string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE threads SET resolved_at = ?, resolved_by = ?, updated_at = ? WHERE id = ?`,
		time.Now().Unix(), byUserID, time.Now().Unix(), threadID)
	return err
}

func (r *Repo) MarkUnresolved(ctx context.Context, threadID string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE threads SET resolved_at = NULL, resolved_by = NULL, updated_at = ? WHERE id = ?`,
		time.Now().Unix(), threadID)
	return err
}

func (r *Repo) TouchThread(ctx context.Context, id string, when time.Time) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE threads SET last_activity_at = ?, updated_at = ? WHERE id = ?`,
		when.Unix(), when.Unix(), id)
	return err
}

// UpdateSubject renames a thread.
func (r *Repo) UpdateSubject(ctx context.Context, id, subject string) error {
	now := time.Now().Unix()
	_, err := r.DB.ExecContext(ctx, `UPDATE threads SET subject = ?, updated_at = ? WHERE id = ?`, subject, now, id)
	return err
}

// HardDeleteThread removes the thread row and all of its posts, returning the
// set of upload IDs referenced anywhere in those bodies so the caller can
// purge the underlying files.
func (r *Repo) HardDeleteThread(ctx context.Context, threadID string) ([]string, error) {
	bodies := []string{}
	var tb string
	if err := r.DB.QueryRowContext(ctx, `SELECT body_md FROM threads WHERE id = ?`, threadID).Scan(&tb); err == nil {
		bodies = append(bodies, tb)
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT body_md FROM posts WHERE thread_id = ?`, threadID)
	if err == nil {
		for rows.Next() {
			var b string
			if rows.Scan(&b) == nil {
				bodies = append(bodies, b)
			}
		}
		rows.Close()
	}
	if _, err := r.DB.ExecContext(ctx, `DELETE FROM posts WHERE thread_id = ?`, threadID); err != nil {
		return nil, err
	}
	if _, err := r.DB.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, threadID); err != nil {
		return nil, err
	}
	return extractUploadIDs(bodies), nil
}

var uploadIDRE = regexp.MustCompile(`/uploads/([0-9a-fA-F-]{36})`)

func extractUploadIDs(bodies []string) []string {
	seen := map[string]struct{}{}
	for _, b := range bodies {
		for _, m := range uploadIDRE.FindAllStringSubmatch(b, -1) {
			seen[m[1]] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
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
		       COALESCE(mb.effective_display_name, ''),
		       COALESCE(qp.body_html, ''), COALESCE(qmb.effective_display_name, ''),
		       p.agent_id, p.bot_name, p.bot_avatar_url, p.gen_status, p.tool_calls
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
		var quoted, agentID sql.NullString
		var del sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&p.ID, &p.ThreadID, &p.AuthorID, &quoted, &p.BodyMarkdown, &p.BodyHTML, &del, &created, &updated, &p.AuthorName, &p.QuotedBody, &p.QuotedAuthor,
			&agentID, &p.BotName, &p.BotAvatar, &p.GenStatus, &p.ToolCalls); err != nil {
			return nil, err
		}
		if quoted.Valid {
			p.QuotedPostID = &quoted.String
		}
		if agentID.Valid {
			p.AgentID = &agentID.String
		}
		// Bot identity overrides the member display whenever the row carries a
		// bot_name — agent replies (agent_id set) and inbound-webhook posts
		// (no agent_id, bot_name = the far-side human) both take this path.
		if p.AgentID != nil || p.BotName != "" {
			p.AuthorName, p.AuthorAvatar = p.BotName, p.BotAvatar
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
	var quoted, agentID sql.NullString
	var del sql.NullInt64
	var created, updated int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, thread_id, author_id, quoted_post_id, body_md, body_html, deleted_at, created_at, updated_at,
		       agent_id, bot_name, bot_avatar_url, gen_status, tool_calls
		FROM posts WHERE id = ?`, id).
		Scan(&p.ID, &p.ThreadID, &p.AuthorID, &quoted, &p.BodyMarkdown, &p.BodyHTML, &del, &created, &updated,
			&agentID, &p.BotName, &p.BotAvatar, &p.GenStatus, &p.ToolCalls)
	if errors.Is(err, sql.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	if err != nil {
		return Post{}, err
	}
	if quoted.Valid {
		p.QuotedPostID = &quoted.String
	}
	if agentID.Valid {
		p.AgentID = &agentID.String
	}
	// See ListPosts: bot_name overrides the member display for agent replies
	// and inbound-webhook posts alike.
	if p.AgentID != nil || p.BotName != "" {
		p.AuthorName, p.AuthorAvatar = p.BotName, p.BotAvatar
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

// AgentBotUserID is the sentinel user that owns every agent reply post's
// NOT-NULL author_id FK (migration 00044). The real identity is the post's
// agent_id / bot_name columns.
const AgentBotUserID = "agent-bot"

// Streaming lifecycle of an agent reply post (Post.GenStatus / gen_status).
const (
	GenGenerating  = "generating"
	GenDone        = "done"
	GenInterrupted = "interrupted"
)

// InsertBotPost inserts an agent reply post (author_id = AgentBotUserID,
// agent_id + bot identity set). The streaming runner inserts it with an empty
// body and gen_status='generating', then rewrites via UpdateBotPostBody.
func (r *Repo) InsertBotPost(ctx context.Context, p Post) error {
	var agentID any
	if p.AgentID != nil {
		agentID = *p.AgentID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO posts (id, thread_id, author_id, body_md, body_html, agent_id, bot_name, bot_avatar_url, gen_status, tool_calls, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ThreadID, p.AuthorID, p.BodyMarkdown, p.BodyHTML, agentID, p.BotName, p.BotAvatar, p.GenStatus, p.ToolCalls,
		p.CreatedAt.Unix(), p.CreatedAt.Unix())
	return err
}

// UpdateBotPostBody rewrites an agent reply post's body + tool trace + streaming
// status as the model streams (called every flush; the thread stream re-renders
// on the broadcast that follows).
func (r *Repo) UpdateBotPostBody(ctx context.Context, postID, md, html, toolCalls, genStatus string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE posts SET body_md = ?, body_html = ?, tool_calls = ?, gen_status = ?, updated_at = ?
		WHERE id = ? AND agent_id IS NOT NULL`,
		md, html, toolCalls, genStatus, time.Now().Unix(), postID)
	return err
}

// MarkBotPostsInterrupted flips lingering 'generating' agent posts to
// 'interrupted' on boot — a restart can't resume an in-flight completion.
func (r *Repo) MarkBotPostsInterrupted(ctx context.Context) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE posts SET gen_status = 'interrupted'
		WHERE agent_id IS NOT NULL AND gen_status = 'generating'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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

func NewService(repo *Repo, grace time.Duration) *Service {
	return &Service{Repo: repo, EditGrace: grace}
}

type CreateThreadInput struct {
	CommunityID  string
	AuthorID     string
	Subject      string
	BodyMarkdown string
	AgentID      *string // set → an agent-owned thread (the agent answers each reply)
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
		AgentID:        in.AgentID,
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

// CreateWebhookThread opens a forum thread on behalf of an inbound webhook
// (e.g. a Matrix thread mirrored in). It is authored by the AgentBot sentinel
// user — threads carry no denormalised bot identity, so the far-side human
// name is surfaced in the opening body instead. subject seeds the title; when
// empty it is derived from the first line of markdown.
func (s *Service) CreateWebhookThread(ctx context.Context, communityID, author, subject, markdown string) (Thread, error) {
	if strings.TrimSpace(markdown) == "" {
		return Thread{}, ErrEmpty
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = firstLineN(markdown, 200)
	}
	if subject == "" {
		subject = "Conversation"
	}
	return s.CreateThread(ctx, CreateThreadInput{
		CommunityID:  communityID,
		AuthorID:     AgentBotUserID,
		Subject:      subject,
		BodyMarkdown: withAuthorPrefix(author, markdown),
	})
}

// CreateWebhookPost appends an inbound webhook message to an existing thread as
// a bot-authored post (author_id = AgentBot sentinel; botName/avatar give it
// the far-side human's display identity). It bumps thread activity. The post
// carries no agent_id, so it is not an AI reply and never re-triggers a
// generation — this is the inbound echo guard.
func (s *Service) CreateWebhookPost(ctx context.Context, threadID, botName, avatar, markdown string) (Post, error) {
	if strings.TrimSpace(markdown) == "" {
		return Post{}, ErrEmpty
	}
	html, err := render.RenderMarkdown(markdown)
	if err != nil {
		return Post{}, err
	}
	now := time.Now()
	p := Post{
		ID:           uuid.NewString(),
		ThreadID:     threadID,
		AuthorID:     AgentBotUserID,
		BodyMarkdown: markdown,
		BodyHTML:     html,
		BotName:      botName,
		BotAvatar:    avatar,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.Repo.InsertBotPost(ctx, p); err != nil {
		return Post{}, err
	}
	if err := s.Repo.TouchThread(ctx, threadID, now); err != nil {
		return Post{}, err
	}
	return p, nil
}

// withAuthorPrefix prepends a bold attribution line when author is set, so a
// webhook-opened thread (whose row is owned by the sentinel) still shows who
// spoke on the far side.
func withAuthorPrefix(author, markdown string) string {
	if author = strings.TrimSpace(author); author == "" {
		return markdown
	}
	return "**" + author + "** (via webhook)\n\n" + markdown
}

// firstLineN returns the first line of s, trimmed to at most n bytes.
func firstLineN(s string, n int) string {
	line := strings.TrimSpace(firstLine(s))
	if len(line) > n {
		line = line[:n]
	}
	return line
}
