package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repo is all SQL for the agent feature. Stateless; reads + writes both live
// here per the project's loose-CQRS shape (service owns orchestration).
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// --- config ---------------------------------------------------------------

// GetConfig returns a community's AI config. A community with no row yet gets
// a zero-value, disabled config (Enabled=false) carrying sane Ollama
// defaults, so callers never special-case "not configured yet".
func (r *Repo) GetConfig(ctx context.Context, communityID string) (Config, error) {
	c := Config{
		CommunityID: communityID,
		Provider:    ProviderOllama,
		BaseURL:     "http://localhost:11434",
		Model:       "llama3.2",
	}
	var enabled int
	err := r.DB.QueryRowContext(ctx, `
		SELECT provider, base_url, model, api_key_enc, system_prompt, enabled,
		       COALESCE(updated_by,''), updated_at
		FROM ai_configs WHERE community_id = ?`, communityID).
		Scan(&c.Provider, &c.BaseURL, &c.Model, &c.APIKeyEnc, &c.SystemPrompt, &enabled, &c.UpdatedBy, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("get config: %w", err)
	}
	c.Enabled = enabled != 0
	return c, nil
}

// SaveConfig upserts a community's AI config.
func (r *Repo) SaveConfig(ctx context.Context, c Config) error {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	var updatedBy any
	if c.UpdatedBy != "" {
		updatedBy = c.UpdatedBy
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_configs (community_id, provider, base_url, model, api_key_enc, system_prompt, enabled, updated_by, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(community_id) DO UPDATE SET
			provider=excluded.provider, base_url=excluded.base_url, model=excluded.model,
			api_key_enc=excluded.api_key_enc, system_prompt=excluded.system_prompt,
			enabled=excluded.enabled, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		c.CommunityID, c.Provider, c.BaseURL, c.Model, c.APIKeyEnc, c.SystemPrompt, enabled, updatedBy, nowUnix())
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// --- threads --------------------------------------------------------------

// CreateThread inserts a new conversation.
func (r *Repo) CreateThread(ctx context.Context, t Thread) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_threads (id, community_id, user_id, visibility, title, model, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.CommunityID, t.UserID, t.Visibility, t.Title, t.Model, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create thread: %w", err)
	}
	return nil
}

// ThreadByID loads one thread. Returns ErrNotFound when absent.
func (r *Repo) ThreadByID(ctx context.Context, id string) (Thread, error) {
	var t Thread
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, user_id, visibility, title, model, created_at, updated_at
		FROM ai_threads WHERE id = ?`, id).
		Scan(&t.ID, &t.CommunityID, &t.UserID, &t.Visibility, &t.Title, &t.Model, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, fmt.Errorf("thread by id: %w", err)
	}
	return t, nil
}

// ListThreads returns the threads visible to userID in communityID: every
// shared thread plus the user's own private threads, newest activity first.
func (r *Repo) ListThreads(ctx context.Context, communityID, userID string) ([]Thread, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, community_id, user_id, visibility, title, model, created_at, updated_at
		FROM ai_threads
		WHERE community_id = ? AND (visibility = 'shared' OR user_id = ?)
		ORDER BY updated_at DESC`, communityID, userID)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.CommunityID, &t.UserID, &t.Visibility, &t.Title, &t.Model, &t.CreatedAt, &t.UpdatedAt); err != nil {
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

// DeleteThread removes a thread; ai_messages cascade.
func (r *Repo) DeleteThread(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM ai_threads WHERE id = ?`, id)
	return err
}

// --- messages -------------------------------------------------------------

// InsertMessage appends a turn.
func (r *Repo) InsertMessage(ctx context.Context, m Message) error {
	var authorID any
	if m.AuthorID != "" {
		authorID = m.AuthorID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_messages (id, thread_id, role, author_id, body_md, body_html, status, error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ThreadID, m.Role, authorID, m.BodyMD, m.BodyHTML, m.Status, m.Error, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// Messages returns a thread's turns oldest-first.
func (r *Repo) Messages(ctx context.Context, threadID string) ([]Message, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, thread_id, role, COALESCE(author_id,''), body_md, body_html, status, error, created_at, updated_at
		FROM ai_messages WHERE thread_id = ? ORDER BY created_at ASC, id ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Role, &m.AuthorID, &m.BodyMD, &m.BodyHTML, &m.Status, &m.Error, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessageByID loads one turn. Returns ErrNotFound when absent.
func (r *Repo) MessageByID(ctx context.Context, id string) (Message, error) {
	var m Message
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, thread_id, role, COALESCE(author_id,''), body_md, body_html, status, error, created_at, updated_at
		FROM ai_messages WHERE id = ?`, id).
		Scan(&m.ID, &m.ThreadID, &m.Role, &m.AuthorID, &m.BodyMD, &m.BodyHTML, &m.Status, &m.Error, &m.CreatedAt, &m.UpdatedAt)
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
