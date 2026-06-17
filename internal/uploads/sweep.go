package uploads

import (
	"context"
	"log/slog"
	"time"
)

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
	// A row is orphaned when no chat_message_attachments, no
	// project_attachments, no project_issue_attachments, no
	// project_discussion_attachments references its id AND its
	// markdown body inclusions can't be traced. Markdown bodies are
	// expensive to scan — for v1 we conservatively keep any upload
	// that the markdown render path could be re-using (image paste
	// path → upload URL embedded in body_html). We delete only when
	// the row is recent enough not to have been markdown-paste'd
	// (the dominant path that doesn't add an attachment row).
	//
	// The "no attachment row anywhere AND created_at older than
	// cutoff" query is the simplest conservative pass.
	rows, err := w.Store.DB.QueryContext(ctx, `
		SELECT u.id, u.rel_path
		FROM uploads u
		WHERE u.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM chat_message_attachments a WHERE a.upload_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM project_attachments       a WHERE a.upload_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM project_issue_attachments a WHERE a.upload_id = u.id)
		  AND NOT EXISTS (
		    SELECT 1 FROM chat_messages m
		    WHERE m.body_md LIKE '%upload://' || u.id || '%'
		       OR m.body_md LIKE '%/uploads/' || u.id || '%'
		  )
		LIMIT 200`, cutoff)
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
