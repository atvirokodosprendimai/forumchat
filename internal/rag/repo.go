package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Repo is the SQL side of RAG: the embed_outbox queue and the content loaders
// that resolve (kind, ref_id) → Doc. The loaders ARE the authorization boundary —
// each query's WHERE clause encodes exactly which rows are community-public
// (e.g. AI messages only from shared, completed threads). A row that no longer
// qualifies returns ok=false, and the worker deletes its vectors.
type Repo struct {
	DB *sql.DB
}

// NewRepo builds a Repo.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// OutboxItem is one pending index job.
type OutboxItem struct {
	Seq   int64
	Kind  string
	RefID string
	Op    string
}

// Dequeue returns up to batch oldest pending jobs (not removed — call Ack after
// processing so a crash mid-batch re-runs the unacked jobs).
func (r *Repo) Dequeue(ctx context.Context, batch int) ([]OutboxItem, error) {
	if batch <= 0 {
		batch = 64
	}
	rows, err := r.DB.QueryContext(ctx,
		`SELECT seq, kind, ref_id, op FROM embed_outbox ORDER BY seq LIMIT ?`, batch)
	if err != nil {
		return nil, fmt.Errorf("dequeue: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.Seq, &it.Kind, &it.RefID, &it.Op); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Ack removes a processed job.
func (r *Repo) Ack(ctx context.Context, seq int64) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM embed_outbox WHERE seq = ?`, seq)
	return err
}

// PendingCount reports how many jobs are queued (for status/logging).
func (r *Repo) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM embed_outbox`).Scan(&n)
	return n, err
}

// loaderSpec is the query that resolves one kind. The query takes ref_id and
// returns community_id, [title,] body_md, created_at — title only when hasTitle.
type loaderSpec struct {
	query    string
	hasTitle bool
}

var loaders = map[string]loaderSpec{
	// Chat: human sends (kind='user') plus FINISHED agent replies
	// (kind='bot' AND gen_status='done'). A bot bubble is public channel
	// content like any other message, so it belongs in the index — but only
	// once streaming completes: a 'generating'/'interrupted' row holds a
	// partial answer, and the final done-UPDATE re-fires the outbox trigger so
	// the completed body lands on the next worker tick. (gen_status is '' for
	// real user messages, hence the kind split rather than a blanket check.)
	KindChat: {query: `SELECT community_id, body_md, created_at FROM chat_messages
		WHERE id = ? AND deleted_at IS NULL
		  AND (kind = 'user' OR (kind = 'bot' AND gen_status = 'done'))`},
	KindThread: {hasTitle: true, query: `SELECT community_id, subject, body_md, created_at FROM threads
		WHERE id = ? AND deleted_at IS NULL`},
	KindPost: {query: `SELECT t.community_id, p.body_md, p.created_at
		FROM posts p JOIN threads t ON t.id = p.thread_id
		WHERE p.id = ? AND p.deleted_at IS NULL`},
	// Project-derived content (issues, comments, discussions, replies, the
	// project itself) is only community-public when the project is community-
	// visible. A restricted project (needs_perms=1 AND visibility='restricted')
	// is hidden from general members, so its content must never enter the
	// community-wide vector index (FIX1 H12). These loaders are the
	// authorization boundary — a row that no longer qualifies returns ok=false
	// and the worker deletes any vectors it had.
	KindIssue: {hasTitle: true, query: `SELECT pr.community_id, i.title, i.body_md, i.created_at
		FROM project_issues i JOIN projects pr ON pr.id = i.project_id
		WHERE i.id = ? AND (pr.needs_perms = 0 OR pr.visibility = 'community')`},
	KindIssueComment: {query: `SELECT pr.community_id, c.body_md, c.created_at
		FROM project_issue_comments c
		JOIN project_issues i ON i.id = c.issue_id
		JOIN projects pr ON pr.id = i.project_id
		WHERE c.id = ? AND c.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')`},
	KindDiscussion: {hasTitle: true, query: `SELECT pr.community_id, d.subject, d.body_md, d.created_at
		FROM project_discussion_threads d JOIN projects pr ON pr.id = d.project_id
		WHERE d.id = ? AND d.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')`},
	KindDiscussionReply: {query: `SELECT pr.community_id, rp.body_md, rp.created_at
		FROM project_discussion_replies rp
		JOIN project_discussion_threads d ON d.id = rp.thread_id
		JOIN projects pr ON pr.id = d.project_id
		WHERE rp.id = ? AND rp.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')`},
	KindProject: {hasTitle: true, query: `SELECT community_id, title, description_md, created_at FROM projects
		WHERE id = ? AND archived_at IS NULL AND (needs_perms = 0 OR visibility = 'community')`},
	// AI assistant turns: only completed turns in SHARED threads are
	// community-public. Private threads are creator-only and must never enter
	// the community-wide index.
	KindAI: {query: `SELECT t.community_id, m.body_md, m.created_at
		FROM ai_messages m JOIN ai_threads t ON t.id = m.thread_id
		WHERE m.id = ? AND m.role = 'assistant' AND m.status = 'done' AND t.visibility = 'shared'`},
	// Pastes: only POSTED pastes are community-public (a draft is unsent,
	// author-private work-in-progress). body is the raw paste source.
	KindPaste: {hasTitle: true, query: `SELECT community_id, title, body, created_at FROM pastes
		WHERE id = ? AND posted_at IS NOT NULL`},
	// Notes: only PUBLIC notes are community-visible (a private note is unlisted,
	// readable only via its share-link). body is the raw markdown source.
	KindNote: {hasTitle: true, query: `SELECT community_id, title, body, created_at FROM notes
		WHERE id = ? AND visibility = 'public'`},
}

// LoadDoc resolves a content row. ok=false means the row is gone or no longer
// community-public — the caller should delete any vectors it had.
func (r *Repo) LoadDoc(ctx context.Context, kind, refID string) (Doc, bool, error) {
	spec, known := loaders[kind]
	if !known {
		return Doc{}, false, nil
	}
	d := Doc{Kind: kind, RefID: refID}
	var err error
	row := r.DB.QueryRowContext(ctx, spec.query, refID)
	if spec.hasTitle {
		err = row.Scan(&d.CommunityID, &d.Title, &d.Body, &d.CreatedAt)
	} else {
		err = row.Scan(&d.CommunityID, &d.Body, &d.CreatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, false, nil
	}
	if err != nil {
		return Doc{}, false, fmt.Errorf("load %s %s: %w", kind, refID, err)
	}
	return d, true, nil
}

// enqueueAll holds one INSERT…SELECT per kind, each enqueuing every currently
// community-public row as an upsert. ON CONFLICT keeps the queue coalesced if a
// trigger already queued the same row.
var enqueueAll = []string{
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'chat', id, 'upsert', ? FROM chat_messages
		WHERE deleted_at IS NULL AND (kind='user' OR (kind='bot' AND gen_status='done'))
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'thread', id, 'upsert', ? FROM threads WHERE deleted_at IS NULL
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'post', id, 'upsert', ? FROM posts WHERE deleted_at IS NULL
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	// Project-derived kinds: only enqueue rows from community-visible projects
	// (FIX1 H12) — matches the loader gate so a reindex doesn't re-add a
	// restricted project's content.
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'issue', i.id, 'upsert', ? FROM project_issues i JOIN projects pr ON pr.id = i.project_id
		WHERE (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'issue_comment', c.id, 'upsert', ? FROM project_issue_comments c
		JOIN project_issues i ON i.id = c.issue_id JOIN projects pr ON pr.id = i.project_id
		WHERE c.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'discussion', d.id, 'upsert', ? FROM project_discussion_threads d JOIN projects pr ON pr.id = d.project_id
		WHERE d.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'discussion_reply', rp.id, 'upsert', ? FROM project_discussion_replies rp
		JOIN project_discussion_threads d ON d.id = rp.thread_id JOIN projects pr ON pr.id = d.project_id
		WHERE rp.deleted_at IS NULL AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'project', id, 'upsert', ? FROM projects WHERE archived_at IS NULL AND (needs_perms = 0 OR visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'ai', m.id, 'upsert', ? FROM ai_messages m JOIN ai_threads t ON t.id = m.thread_id
		WHERE m.role='assistant' AND m.status='done' AND t.visibility='shared'
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'paste', id, 'upsert', ? FROM pastes WHERE posted_at IS NOT NULL
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'note', id, 'upsert', ? FROM notes WHERE visibility = 'public'
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
}

// EnqueueAll queues every community-public row across all communities for
// re-embedding (global reindex). Returns the resulting pending-queue size.
func (r *Repo) EnqueueAll(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	for _, stmt := range enqueueAll {
		if _, err := r.DB.ExecContext(ctx, stmt, now); err != nil {
			return 0, fmt.Errorf("enqueue all: %w", err)
		}
	}
	return r.PendingCount(ctx)
}

// enqueueCommunity mirrors enqueueAll but scoped to one community. Each statement
// takes (enqueued_at, community_id).
var enqueueCommunity = []string{
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'chat', id, 'upsert', ? FROM chat_messages
		WHERE deleted_at IS NULL AND community_id = ? AND (kind='user' OR (kind='bot' AND gen_status='done'))
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'thread', id, 'upsert', ? FROM threads WHERE deleted_at IS NULL AND community_id = ?
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'post', p.id, 'upsert', ? FROM posts p JOIN threads t ON t.id = p.thread_id
		WHERE p.deleted_at IS NULL AND t.community_id = ?
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'issue', i.id, 'upsert', ? FROM project_issues i JOIN projects pr ON pr.id = i.project_id
		WHERE pr.community_id = ? AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'issue_comment', c.id, 'upsert', ? FROM project_issue_comments c
		JOIN project_issues i ON i.id = c.issue_id JOIN projects pr ON pr.id = i.project_id
		WHERE c.deleted_at IS NULL AND pr.community_id = ? AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'discussion', d.id, 'upsert', ? FROM project_discussion_threads d JOIN projects pr ON pr.id = d.project_id
		WHERE d.deleted_at IS NULL AND pr.community_id = ? AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'discussion_reply', rp.id, 'upsert', ? FROM project_discussion_replies rp
		JOIN project_discussion_threads d ON d.id = rp.thread_id JOIN projects pr ON pr.id = d.project_id
		WHERE rp.deleted_at IS NULL AND pr.community_id = ? AND (pr.needs_perms = 0 OR pr.visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'project', id, 'upsert', ? FROM projects WHERE archived_at IS NULL AND community_id = ? AND (needs_perms = 0 OR visibility = 'community')
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'ai', m.id, 'upsert', ? FROM ai_messages m JOIN ai_threads t ON t.id = m.thread_id
		WHERE m.role='assistant' AND m.status='done' AND t.visibility='shared' AND t.community_id = ?
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'paste', id, 'upsert', ? FROM pastes WHERE posted_at IS NOT NULL AND community_id = ?
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
	`INSERT INTO embed_outbox(kind, ref_id, op, enqueued_at)
		SELECT 'note', id, 'upsert', ? FROM notes WHERE visibility = 'public' AND community_id = ?
		ON CONFLICT(kind, ref_id) DO UPDATE SET op='upsert', enqueued_at=excluded.enqueued_at`,
}

// EnqueueCommunity queues every community-public row for one community.
func (r *Repo) EnqueueCommunity(ctx context.Context, communityID string) (int, error) {
	now := time.Now().Unix()
	for _, stmt := range enqueueCommunity {
		if _, err := r.DB.ExecContext(ctx, stmt, now, communityID); err != nil {
			return 0, fmt.Errorf("enqueue community: %w", err)
		}
	}
	return r.PendingCount(ctx)
}

// atoi64 parses a metadata string back to int64, 0 on error.
func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
