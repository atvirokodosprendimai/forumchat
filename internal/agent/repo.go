package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Repo is all SQL for the agent feature. Stateless; reads + writes both live
// here per the project's loose-CQRS shape (service owns orchestration).
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// --- agents ---------------------------------------------------------------

const agentCols = `id, community_id, name, provider, base_url, model, api_key_enc,
	system_prompt, vision, tools_enabled, enabled, is_summarizer,
	in_chat_enabled, trigger_mode, trigger_prefix, avatar_url,
	position, COALESCE(updated_by,''), created_at, updated_at`

func scanAgent(s interface {
	Scan(dest ...any) error
}) (Agent, error) {
	var a Agent
	var vision, tools, enabled, summarizer, inChat int
	err := s.Scan(&a.ID, &a.CommunityID, &a.Name, &a.Provider, &a.BaseURL, &a.Model, &a.APIKeyEnc,
		&a.SystemPrompt, &vision, &tools, &enabled, &summarizer,
		&inChat, &a.TriggerMode, &a.TriggerPrefix, &a.AvatarURL,
		&a.Position, &a.UpdatedBy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Agent{}, err
	}
	a.Vision = vision != 0
	a.ToolsEnabled = tools != 0
	a.Enabled = enabled != 0
	a.IsSummarizer = summarizer != 0
	a.InChatEnabled = inChat != 0
	return a, nil
}

// ListAgents returns a community's agents in display order.
func (r *Repo) ListAgents(ctx context.Context, communityID string) ([]Agent, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+agentCols+`
		FROM ai_agents WHERE community_id = ? ORDER BY position, name`, communityID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListEnabledAgents returns only the agents members may chat with.
func (r *Repo) ListEnabledAgents(ctx context.Context, communityID string) ([]Agent, error) {
	all, err := r.ListAgents(ctx, communityID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

// AgentByID loads one agent. Returns ErrNotFound when absent.
func (r *Repo) AgentByID(ctx context.Context, id string) (Agent, error) {
	a, err := scanAgent(r.DB.QueryRowContext(ctx, `SELECT `+agentCols+` FROM ai_agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("agent by id: %w", err)
	}
	return a, nil
}

// SummarizerAgent returns the enabled agent a community marked to handle the
// chat /summary channel summary. ErrNotFound when none is marked (or the marked
// one is disabled) — callers fall back to the first enabled agent.
func (r *Repo) SummarizerAgent(ctx context.Context, communityID string) (Agent, error) {
	a, err := scanAgent(r.DB.QueryRowContext(ctx, `SELECT `+agentCols+`
		FROM ai_agents WHERE community_id = ? AND is_summarizer = 1 AND enabled = 1
		ORDER BY position, name LIMIT 1`, communityID))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("summarizer agent: %w", err)
	}
	return a, nil
}

// ClearOtherSummarizers unsets the summarizer flag on every agent in the
// community except keepID, enforcing the one-summarizer-per-community invariant.
func (r *Repo) ClearOtherSummarizers(ctx context.Context, communityID, keepID string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE ai_agents SET is_summarizer = 0
		WHERE community_id = ? AND id <> ?`, communityID, keepID)
	if err != nil {
		return fmt.Errorf("clear other summarizers: %w", err)
	}
	return nil
}

// CountAgents returns how many agents a community has.
func (r *Repo) CountAgents(ctx context.Context, communityID string) (int, error) {
	var n int
	err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_agents WHERE community_id = ?`, communityID).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateAgent inserts a new agent.
func (r *Repo) CreateAgent(ctx context.Context, a Agent) error {
	var updatedBy any
	if a.UpdatedBy != "" {
		updatedBy = a.UpdatedBy
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_agents (id, community_id, name, provider, base_url, model, api_key_enc,
			system_prompt, vision, tools_enabled, enabled, is_summarizer,
			in_chat_enabled, trigger_mode, trigger_prefix, avatar_url,
			position, created_at, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.CommunityID, a.Name, a.Provider, a.BaseURL, a.Model, a.APIKeyEnc,
		a.SystemPrompt, boolToInt(a.Vision), boolToInt(a.ToolsEnabled), boolToInt(a.Enabled), boolToInt(a.IsSummarizer),
		boolToInt(a.InChatEnabled), agentTriggerMode(a.TriggerMode), agentTriggerPrefix(a.TriggerPrefix), a.AvatarURL,
		a.Position, a.CreatedAt, a.UpdatedAt, updatedBy)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	return nil
}

// UpdateAgent rewrites an agent's mutable fields.
func (r *Repo) UpdateAgent(ctx context.Context, a Agent) error {
	var updatedBy any
	if a.UpdatedBy != "" {
		updatedBy = a.UpdatedBy
	}
	_, err := r.DB.ExecContext(ctx, `
		UPDATE ai_agents SET name=?, provider=?, base_url=?, model=?, api_key_enc=?,
			system_prompt=?, vision=?, tools_enabled=?, enabled=?, is_summarizer=?,
			in_chat_enabled=?, trigger_mode=?, trigger_prefix=?, avatar_url=?, updated_at=?, updated_by=?
		WHERE id = ?`,
		a.Name, a.Provider, a.BaseURL, a.Model, a.APIKeyEnc, a.SystemPrompt,
		boolToInt(a.Vision), boolToInt(a.ToolsEnabled), boolToInt(a.Enabled), boolToInt(a.IsSummarizer),
		boolToInt(a.InChatEnabled), agentTriggerMode(a.TriggerMode), agentTriggerPrefix(a.TriggerPrefix), a.AvatarURL,
		a.UpdatedAt, updatedBy, a.ID)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	return nil
}

// DeleteAgent removes an agent; its threads cascade.
func (r *Repo) DeleteAgent(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM ai_agents WHERE id = ?`, id)
	return err
}

// agentTriggerMode normalises a trigger_mode to a known value, defaulting to
// mention so the CHECK constraint never rejects an unset field.
func agentTriggerMode(mode string) string {
	switch mode {
	case TriggerModeMention, TriggerModePrefix, TriggerModeBoth, TriggerModeAll:
		return mode
	default:
		return TriggerModeMention
	}
}

// agentTriggerPrefix defaults an empty prefix to ".".
func agentTriggerPrefix(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// --- chat participation ---------------------------------------------------

// AgentsForChannel returns the enabled, chat-participating agents bound to a
// channel — the set the roster, mention autocomplete, and trigger dispatch use.
func (r *Repo) AgentsForChannel(ctx context.Context, communityID, channelID string) ([]Agent, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+agentCols+`
		FROM ai_agents a
		JOIN ai_agent_channels ac ON ac.agent_id = a.id
		WHERE a.community_id = ? AND ac.channel_id = ?
		  AND a.enabled = 1 AND a.in_chat_enabled = 1
		ORDER BY a.position, a.name`, communityID, channelID)
	if err != nil {
		return nil, fmt.Errorf("agents for channel: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListInChatAgents returns every enabled, chat-participating agent in a
// community — the roster's always-online bot set (channel-agnostic, mirroring
// the community-wide member roster).
func (r *Repo) ListInChatAgents(ctx context.Context, communityID string) ([]Agent, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+agentCols+`
		FROM ai_agents
		WHERE community_id = ? AND enabled = 1 AND in_chat_enabled = 1
		ORDER BY position, name`, communityID)
	if err != nil {
		return nil, fmt.Errorf("list in-chat agents: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ChannelIDsForAgent returns the channel ids an agent is bound to.
func (r *Repo) ChannelIDsForAgent(ctx context.Context, agentID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT channel_id FROM ai_agent_channels WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, fmt.Errorf("channels for agent: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetAgentChannels replaces an agent's channel bindings in one transaction.
func (r *Repo) SetAgentChannels(ctx context.Context, agentID string, channelIDs []string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_agent_channels WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear agent channels: %w", err)
	}
	for _, cid := range channelIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO ai_agent_channels (agent_id, channel_id) VALUES (?, ?)`,
			agentID, cid); err != nil {
			return fmt.Errorf("bind agent channel: %w", err)
		}
	}
	return tx.Commit()
}

// --- threads --------------------------------------------------------------

const threadCols = `id, community_id, user_id, COALESCE(agent_id,''), visibility, title, model, created_at, updated_at`

func scanThread(s interface {
	Scan(dest ...any) error
}) (Thread, error) {
	var t Thread
	err := s.Scan(&t.ID, &t.CommunityID, &t.UserID, &t.AgentID, &t.Visibility, &t.Title, &t.Model, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// CreateThread inserts a new conversation.
func (r *Repo) CreateThread(ctx context.Context, t Thread) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_threads (id, community_id, user_id, agent_id, visibility, title, model, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		t.ID, t.CommunityID, t.UserID, t.AgentID, t.Visibility, t.Title, t.Model, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create thread: %w", err)
	}
	return nil
}

// ThreadByID loads one thread. Returns ErrNotFound when absent.
func (r *Repo) ThreadByID(ctx context.Context, id string) (Thread, error) {
	t, err := scanThread(r.DB.QueryRowContext(ctx, `SELECT `+threadCols+` FROM ai_threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, fmt.Errorf("thread by id: %w", err)
	}
	return t, nil
}

// ListThreads returns the threads visible to userID: every shared thread plus
// the user's own private threads, newest activity first.
func (r *Repo) ListThreads(ctx context.Context, communityID, userID string) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+threadCols+`
		FROM ai_threads
		WHERE community_id = ? AND (visibility = 'shared' OR user_id = ?)
		ORDER BY updated_at DESC`, communityID, userID)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SearchThreads finds the threads visible to userID whose title matches q
// (case-insensitive substring), newest first. Powers the $-reference
// autocomplete in the agent composer.
func (r *Repo) SearchThreads(ctx context.Context, communityID, userID, q string, limit int) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+threadCols+`
		FROM ai_threads
		WHERE community_id = ? AND (visibility = 'shared' OR user_id = ?) AND title LIKE ? ESCAPE '\'
		ORDER BY updated_at DESC LIMIT ?`,
		communityID, userID, "%"+escapeLike(q)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("search threads: %w", err)
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE wildcards so user input is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// TouchThread bumps updated_at so the thread floats to the top of the list.
func (r *Repo) TouchThread(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE ai_threads SET updated_at = ? WHERE id = ?`, nowUnix(), id)
	return err
}

// SetThreadTitle renames a thread (used to auto-title from the first prompt).
func (r *Repo) SetThreadTitle(ctx context.Context, id, title string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE ai_threads SET title = ? WHERE id = ?`, title, id)
	return err
}

// SetThreadAgent repoints a thread to a different agent (in-thread model
// switch). Subsequent turns use the new agent's provider/model/system-prompt;
// the visible history is unchanged.
func (r *Repo) SetThreadAgent(ctx context.Context, id, agentID, model string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE ai_threads SET agent_id = ?, model = ?, updated_at = ? WHERE id = ?`,
		agentID, model, nowUnix(), id)
	return err
}

// DeleteThread removes a thread; ai_messages cascade.
func (r *Repo) DeleteThread(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM ai_threads WHERE id = ?`, id)
	return err
}

// --- messages -------------------------------------------------------------

func encodeImages(imgs []string) string {
	if len(imgs) == 0 {
		return ""
	}
	b, err := json.Marshal(imgs)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeImages(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// InsertMessage appends a turn.
func (r *Repo) InsertMessage(ctx context.Context, m Message) error {
	var authorID any
	if m.AuthorID != "" {
		authorID = m.AuthorID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_messages (id, thread_id, role, author_id, body_md, body_html, status, error, images, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ThreadID, m.Role, authorID, m.BodyMD, m.BodyHTML, m.Status, m.Error, encodeImages(m.Images), m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

const msgCols = `id, thread_id, role, COALESCE(author_id,''), body_md, body_html, status, error, images, tool_calls, created_at, updated_at`

func scanMessage(s interface {
	Scan(dest ...any) error
}) (Message, error) {
	var m Message
	var images, toolCalls string
	err := s.Scan(&m.ID, &m.ThreadID, &m.Role, &m.AuthorID, &m.BodyMD, &m.BodyHTML, &m.Status, &m.Error, &images, &toolCalls, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Message{}, err
	}
	m.Images = decodeImages(images)
	m.ToolCalls = decodeToolCalls(toolCalls)
	return m, nil
}

// Messages returns a thread's turns oldest-first.
func (r *Repo) Messages(ctx context.Context, threadID string) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+msgCols+`
		FROM ai_messages WHERE thread_id = ? ORDER BY created_at ASC, id ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessageByID loads one turn. Returns ErrNotFound when absent.
func (r *Repo) MessageByID(ctx context.Context, id string) (Message, error) {
	m, err := scanMessage(r.DB.QueryRowContext(ctx, `SELECT `+msgCols+` FROM ai_messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("message by id: %w", err)
	}
	return m, nil
}

// UpdateAssistantBody rewrites a streaming assistant row. Called on every
// 100ms flush (status=generating) and once at the end (done/interrupted/error).
// toolCalls is the JSON trace of any MCP tool calls this turn made ("" if none).
func (r *Repo) UpdateAssistantBody(ctx context.Context, id, md, html, status, errStr, toolCalls string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE ai_messages SET body_md = ?, body_html = ?, status = ?, error = ?, tool_calls = ?, updated_at = ?
		WHERE id = ?`, md, html, status, errStr, toolCalls, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("update assistant body: %w", err)
	}
	return nil
}

// SearchContent runs the internal full-text search ("internal MCP") over a
// community's chat + forum content (the search_fts index). Returns ranked hits
// (best match first). Tokens are quoted so arbitrary model/user input never
// trips FTS5 query syntax.
func (r *Repo) SearchContent(ctx context.Context, communityID, query string, limit int) ([]SearchHit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT kind, ref_id, title, snippet(search_fts, 1, '«', '»', '…', 12), created_at
		FROM search_fts
		WHERE community_id = ? AND search_fts MATCH ?
		ORDER BY bm25(search_fts) LIMIT ?`,
		communityID, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search content: %w", err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Kind, &h.RefID, &h.Title, &h.Snippet, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: each whitespace
// token becomes a double-quoted term (implicit AND), so punctuation and FTS5
// operators in the input are treated literally instead of erroring.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " ")
}

// MarkGeneratingInterrupted flips every still-"generating" assistant row to
// "interrupted". Run once at boot: a process restart orphaned those runners,
// and an LLM completion can't be resumed mid-stream — the persisted partial
// stays, the UI offers Regenerate. Returns the number of rows healed.
func (r *Repo) MarkGeneratingInterrupted(ctx context.Context) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE ai_messages SET status = ?, updated_at = ? WHERE status = ?`,
		StatusInterrupted, nowUnix(), StatusGenerating)
	if err != nil {
		return 0, fmt.Errorf("heal generating: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
