// Package debuglog is the platform-wide debug recorder: an in-memory on/off
// switch (off by default, NOT persisted — it resets on restart) that, when on,
// captures integration payloads (webhook inbound/outbound, etc.) into the
// debug_logs table. The switch and the captured rows are surfaced only on the
// super-admin /superadmin/debug page. It is a leaf infrastructure package — it
// imports nothing else in this codebase, so any package may record into it.
package debuglog

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// maxPayloadBytes caps a stored payload so an inbound 1 MiB webhook body can't
// bloat a debug row. Anything longer is truncated with a marker appended.
const maxPayloadBytes = 64 << 10

// listLimit caps how many recent entries the viewer page loads.
const listLimit = 300

// Recorder gates and persists debug entries. The enabled flag lives in memory
// only (atomic.Bool); a process restart returns it to off. The zero value is
// not usable — construct with New. All methods are safe on a nil receiver, so
// callers can hold a *Recorder that is nil when the feature is unwired.
type Recorder struct {
	db      *sql.DB
	log     *slog.Logger
	enabled atomic.Bool
}

// New returns a Recorder backed by db. It starts disabled.
func New(db *sql.DB, log *slog.Logger) *Recorder {
	return &Recorder{db: db, log: log}
}

// Enabled reports whether recording is currently on.
func (r *Recorder) Enabled() bool {
	return r != nil && r.enabled.Load()
}

// SetEnabled flips the in-memory switch. Returns the new state.
func (r *Recorder) SetEnabled(on bool) bool {
	if r == nil {
		return false
	}
	r.enabled.Store(on)
	return on
}

// Entry is one stored debug row, newest-first from List.
type Entry struct {
	ID        string
	CreatedAt time.Time
	Source    string
	Event     string
	Summary   string
	Payload   string
	Meta      string
}

// Record persists one debug entry when recording is enabled; it is a cheap
// no-op (and nil-safe) otherwise, so call sites need no guard. payload is
// stored verbatim up to maxPayloadBytes (then truncated); meta is marshalled to
// JSON for context. A write failure is logged, never returned — debugging
// capture must not affect the request it is observing.
func (r *Recorder) Record(ctx context.Context, source, event, summary string, payload []byte, meta map[string]string) {
	if !r.Enabled() {
		return
	}
	body := string(payload)
	if len(body) > maxPayloadBytes {
		body = body[:maxPayloadBytes] + "\n…[truncated]"
	}
	metaJSON := ""
	if len(meta) > 0 {
		if b, err := json.Marshal(meta); err == nil {
			metaJSON = string(b)
		}
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO debug_logs (id, source, event, summary, payload, meta)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), source, event, summary, body, metaJSON)
	if err != nil && r.log != nil {
		r.log.Warn("debuglog: record", "err", err, "source", source, "event", event)
	}
}

// List returns the most recent entries, newest first, capped at listLimit.
func (r *Recorder) List(ctx context.Context) ([]Entry, error) {
	if r == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, created_at, source, event, summary, payload, meta
		 FROM debug_logs ORDER BY created_at DESC, id DESC LIMIT ?`, listLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Source, &e.Event, &e.Summary, &e.Payload, &e.Meta); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Count returns the total number of stored entries.
func (r *Recorder) Count(ctx context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM debug_logs`).Scan(&n)
	return n, err
}

// Clear deletes every stored entry. The in-memory switch is untouched.
func (r *Recorder) Clear(ctx context.Context) error {
	if r == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM debug_logs`)
	return err
}
