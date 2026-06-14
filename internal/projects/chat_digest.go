package projects

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// ChatDigestWorker periodically scans each community for projects that
// have had child writes (attachments, issues, discussions, issue
// comments, discussion replies) since the last digest tick, and posts
// ONE consolidated chat message per community listing the changed
// projects. Bumping last_at after each post means the next tick only
// reports genuinely-new changes; users opted into chat see the digest
// in the same place they read everything else.
//
// Wiring:
//   (&projects.ChatDigestWorker{DB, ChatRepo, ChatBus, Communities,
//                              IntervalMinutes: cfg.X, BaseURL, Log}).Start(ctx)
//   - Interval <= 0 disables the worker entirely.
//   - Pass the same chat.Repo + Bus the rest of the app uses so the
//     posted message lands in the existing realtime pipeline.
type ChatDigestWorker struct {
	DB              *sql.DB
	ChatRepo        *chat.Repo
	ChatBus         *chat.Bus
	BaseURL         string
	IntervalMinutes int
	Log             *slog.Logger
}

type communityRow struct {
	ID   string
	Slug string
	Name string
}

type changedProject struct {
	ID    string
	Title string
}

func (w *ChatDigestWorker) Start(ctx context.Context) {
	if w.IntervalMinutes <= 0 {
		if w.Log != nil {
			w.Log.Info("projects chat-digest worker disabled (interval = 0)")
		}
		return
	}
	interval := time.Duration(w.IntervalMinutes) * time.Minute
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
	if w.Log != nil {
		w.Log.Info("projects chat-digest worker started", "interval_min", w.IntervalMinutes)
	}
}

func (w *ChatDigestWorker) tick(ctx context.Context) {
	comms, err := w.listCommunities(ctx)
	if err != nil {
		w.Log.Warn("chat-digest list communities", "err", err)
		return
	}
	nowMs := time.Now().UnixMilli()
	for _, c := range comms {
		lastAt, err := w.lastAt(ctx, c.ID, nowMs)
		if err != nil {
			w.Log.Warn("chat-digest last_at", "community", c.ID, "err", err)
			continue
		}
		if lastAt == nowMs {
			// First-ever encounter: initialised to now() so we don't spam
			// chat with old history. Nothing to report this round.
			continue
		}
		changed, err := w.changedProjects(ctx, c.ID, lastAt)
		if err != nil {
			w.Log.Warn("chat-digest changed scan", "community", c.ID, "err", err)
			continue
		}
		if len(changed) > 0 {
			if err := w.postDigest(ctx, c, changed); err != nil {
				w.Log.Warn("chat-digest post", "community", c.ID, "err", err)
				continue
			}
		}
		if err := w.bumpLastAt(ctx, c.ID, nowMs); err != nil {
			w.Log.Warn("chat-digest bump last_at", "community", c.ID, "err", err)
		}
	}
}

func (w *ChatDigestWorker) listCommunities(ctx context.Context) ([]communityRow, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT id, slug, name FROM communities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []communityRow
	for rows.Next() {
		var c communityRow
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// lastAt returns the previously-stored cursor or, when no row exists
// yet, inserts one initialised to now and returns now so the very first
// tick after install never emits the entire pre-existing history.
func (w *ChatDigestWorker) lastAt(ctx context.Context, communityID string, nowMs int64) (int64, error) {
	var lastAt int64
	err := w.DB.QueryRowContext(ctx,
		`SELECT last_at FROM project_chat_digest_state WHERE community_id = ?`, communityID,
	).Scan(&lastAt)
	if err == nil {
		return lastAt, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	_, err = w.DB.ExecContext(ctx, `
		INSERT INTO project_chat_digest_state (community_id, last_at, updated_at)
		VALUES (?, ?, ?)
	`, communityID, nowMs, nowMs)
	if err != nil {
		return 0, err
	}
	return nowMs, nil
}

func (w *ChatDigestWorker) bumpLastAt(ctx context.Context, communityID string, ts int64) error {
	_, err := w.DB.ExecContext(ctx, `
		UPDATE project_chat_digest_state
		SET last_at = ?, updated_at = ?
		WHERE community_id = ?
	`, ts, ts, communityID)
	return err
}

// changedProjects returns every project in the community that has
// experienced at least one child write since `since` (unix millis).
// OR-of-EXISTS keeps the row count tiny (each EXISTS short-circuits on
// the first matching child) at the cost of one DB query per community.
func (w *ChatDigestWorker) changedProjects(ctx context.Context, communityID string, since int64) ([]changedProject, error) {
	const q = `
		SELECT p.id, p.title
		FROM projects p
		WHERE p.community_id = ?
		  AND (
		     p.updated_at > ?
		  OR EXISTS (SELECT 1 FROM project_attachments a       WHERE a.project_id = p.id AND a.created_at > ?)
		  OR EXISTS (SELECT 1 FROM project_issues i            WHERE i.project_id = p.id AND i.updated_at > ?)
		  OR EXISTS (SELECT 1 FROM project_discussion_threads d       WHERE d.project_id = p.id AND d.updated_at > ?)
		  OR EXISTS (
		       SELECT 1 FROM project_issue_comments pic
		       JOIN project_issues pi ON pic.issue_id = pi.id
		       WHERE pi.project_id = p.id AND pic.created_at > ?
		     )
		  OR EXISTS (
		       SELECT 1 FROM project_discussion_replies pdr
		       JOIN project_discussion_threads pd ON pdr.thread_id = pd.id
		       WHERE pd.project_id = p.id AND pdr.created_at > ?
		     )
		  )
		ORDER BY p.title ASC
	`
	rows, err := w.DB.QueryContext(ctx, q, communityID, since, since, since, since, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changedProject
	for rows.Next() {
		var p changedProject
		if err := rows.Scan(&p.ID, &p.Title); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (w *ChatDigestWorker) postDigest(ctx context.Context, c communityRow, projects []changedProject) error {
	base := strings.TrimRight(w.BaseURL, "/")
	links := make([]string, 0, len(projects))
	plain := make([]string, 0, len(projects))
	for _, p := range projects {
		safeName := htmlEsc(p.Title)
		safeHref := htmlEsc(base + "/c/" + c.Slug + "/projects/" + p.ID)
		links = append(links, `<a href="`+safeHref+`" target="_blank" rel="noopener">`+safeName+`</a>`)
		plain = append(plain, "["+p.Title+"]")
	}
	var bodyHTML string
	if len(projects) == 1 {
		bodyHTML = "📂 Project " + links[0] + " has changes."
	} else {
		bodyHTML = "📂 Updates in projects: " + strings.Join(links, ", ")
	}
	bodyMD := "📂 Updates in projects: " + strings.Join(plain, ", ")
	msg := chat.Message{
		ID:           uuid.NewString(),
		CommunityID:  c.ID,
		AuthorID:     nil, // system message
		Kind:         chat.KindSystem,
		BodyMarkdown: bodyMD,
		BodyHTML:     bodyHTML,
		CreatedAt:    time.Now(),
	}
	if err := w.ChatRepo.Insert(ctx, msg); err != nil {
		return err
	}
	w.ChatBus.Broadcast()
	return nil
}

// htmlEsc is a local copy so this file doesn't pull in the projects
// handler.go helper (different package layout reasons — the worker
// lives in the same package but we want it independent of handler).
func htmlEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
