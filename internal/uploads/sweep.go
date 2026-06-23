package uploads

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// bodyHTMLTables lists every table whose rendered body_html can embed an upload
// URL (image paste / inline media writes a reference into the body, NOT an
// attachment-link row). The sweep must check all of them — missing one means a
// still-referenced upload gets deleted (data loss). All names are internal
// constants (never user input), so the interpolation is injection-free, exactly
// like internal/dataexport's manifest.
var bodyHTMLTables = []string{
	"chat_messages", "threads", "posts", "lobby_messages", "private_messages",
	"project_comments", "project_discussion_threads", "project_discussion_replies",
	"project_issues", "project_issue_comments", "room_chat", "room_chat_archive",
	"pastes", "ai_messages",
	// email_ingest has body_html (migration 00024) but no write path populates
	// it today — listed preemptively so a future ingest-HTML path can't make the
	// sweep delete a still-referenced upload.
	"email_ingest",
}

// SweepWorker periodically deletes upload rows that no row anywhere in
// the database references. Phase-3 of the chat-attachments work made
// it cheap to upload a file and then never link it (e.g. user picks a
// file, the upload completes, but they close the tab before clicking
// Send) — without a sweep those orphans accumulate forever.
//
// The sweep is conservative: only rows older than `MinAge` (defaults
// to 24h) get considered, so an upload-in-progress (between PostUpload
// returning and PostSend running) is never collected.
type SweepWorker struct {
	Store    *Store
	Interval time.Duration // poll cadence; 1h is a fine default
	MinAge   time.Duration // upload age before it counts as orphan
	Log      *slog.Logger
}

// Start runs until ctx is cancelled. Safe to call once at boot.
func (w *SweepWorker) Start(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = time.Hour
	}
	if w.MinAge <= 0 {
		w.MinAge = 24 * time.Hour
	}
	go func() {
		// First run after a brief delay so the freshly-applied boot
		// migrations don't contend with the sweep query.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		w.tick(ctx)
		t := time.NewTicker(w.Interval)
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
}

func (w *SweepWorker) tick(ctx context.Context) {
	if w.Store == nil {
		return
	}
	cutoff := time.Now().Add(-w.MinAge).Unix()
	rows, err := w.Store.DB.QueryContext(ctx, orphanQuery(), cutoff)
	if err != nil {
		w.Log.Warn("uploads sweep query", "err", err)
		return
	}
	type victim struct {
		id      string
		relPath string
	}
	var vics []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.relPath); err != nil {
			continue
		}
		vics = append(vics, v)
	}
	rows.Close()
	if len(vics) == 0 {
		return
	}
	for _, v := range vics {
		if err := w.Store.Delete(ctx, v.id); err != nil {
			w.Log.Warn("uploads sweep delete", "id", v.id, "err", err)
		}
	}
	w.Log.Info("uploads sweep", "deleted", len(vics))
}

// orphanQuery builds the orphan-detection SQL. A row is orphaned only when NO
// attachment-link row references it AND its id appears in no body_html anywhere
// (image paste / inline media embeds a reference into the body without an
// attachment row). Both the resolved signed URL (/uploads/<id>?sig=…) and the
// raw upload://<id> form contain the id in body_html, so one LIKE per table
// catches both. Table names are internal constants — injection-free.
func orphanQuery() string {
	var q strings.Builder
	q.WriteString(`
		SELECT u.id, u.rel_path
		FROM uploads u
		WHERE u.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM chat_message_attachments a WHERE a.upload_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM project_attachments       a WHERE a.upload_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM project_issue_attachments a WHERE a.upload_id = u.id)`)
	for _, tbl := range bodyHTMLTables {
		fmt.Fprintf(&q, `
		  AND NOT EXISTS (SELECT 1 FROM %s b WHERE b.body_html LIKE '%%/uploads/' || u.id || '%%' OR b.body_html LIKE '%%upload://' || u.id || '%%')`, tbl)
	}
	q.WriteString(`
		LIMIT 200`)
	return q.String()
}
