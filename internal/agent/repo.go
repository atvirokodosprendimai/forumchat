package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Repo is all SQL for the agent feature. Stateless; reads + writes both live
// here per the project's loose-CQRS shape (service owns orchestration).
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// --- agents ---------------------------------------------------------------

const agentCols = `id, community_id, name, provider, base_url, model, api_key_enc,
	system_prompt, vision, enabled, position, COALESCE(updated_by,''), created_at, updated_at`

func scanAgent(s interface {
	Scan(dest ...any) error
}) (Agent, error) {
	var a Agent
	var vision, enabled int
	err := s.Scan(&a.ID, &a.CommunityID, &a.Name, &a.Provider, &a.BaseURL, &a.Model, &a.APIKeyEnc,
		&a.SystemPrompt, &vision, &enabled, &a.Position, &a.UpdatedBy, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Agent{}, err
	}
	a.Vision = vision != 0
	a.Enabled = enabled != 0
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
			system_prompt, vision, enabled, position, created_at, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.CommunityID, a.Name, a.Provider, a.BaseURL, a.Model, a.APIKeyEnc,
		a.SystemPrompt, boolToInt(a.Vision), boolToInt(a.Enabled), a.Position, a.CreatedAt, a.UpdatedAt, updatedBy)
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
			system_prompt=?, vision=?, enabled=?, updated_at=?, updated_by=?
		WHERE id = ?`,
		a.Name, a.Provider, a.BaseURL, a.Model, a.APIKeyEnc, a.SystemPrompt,
		boolToInt(a.Vision), boolToInt(a.Enabled), a.UpdatedAt, updatedBy, a.ID)
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

const msgCols = `id, thread_id, role, COALESCE(author_id,''), body_md, body_html, status, error, images, created_at, updated_at`

func scanMessage(s interface {
	Scan(dest ...any) error
}) (Message, error) {
	var m Message
	var images string
	err := s.Scan(&m.ID, &m.ThreadID, &m.Role, &m.AuthorID, &m.BodyMD, &m.BodyHTML, &m.Status, &m.Error, &images, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Message{}, err
	}
	m.Images = decodeImages(images)
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
func (r *Repo) UpdateAssistantBody(ctx context.Context, id, md, html, status, errStr string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE ai_messages SET body_md = ?, body_html = ?, status = ?, error = ?, updated_at = ?
		WHERE id = ?`, md, html, status, errStr, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("update assistant body: %w", err)
	}
	return nil
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
