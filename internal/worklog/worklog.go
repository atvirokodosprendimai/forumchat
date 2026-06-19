// Package worklog implements a global, per-user work timer plus a personal
// journal. A user starts a single timer (server stamps started_at), watches
// the elapsed time tick, and on stop is asked "what did you do?" — the answer
// becomes a journal entry. It is NOT community-scoped: the timer and journal
// follow the user across every community they belong to.
//
// The DB enforces at-most-one running session per user via a partial unique
// index on (user_id) WHERE ended_at IS NULL, so a double-start can never
// create two live timers.
package worklog

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when there is no matching session (e.g. stopping
// with no timer running).
var ErrNotFound = errors.New("worklog: not found")

// Session is one timer run. EndedAt is nil while the timer is running.
type Session struct {
	ID        string
	UserID    string
	StartedAt time.Time
	EndedAt   *time.Time
	Note      string
	CreatedAt time.Time
}

// DurationMinutes returns the elapsed whole minutes for a completed session
// (0 while still running).
func (s Session) DurationMinutes() int {
	if s.EndedAt == nil {
		return 0
	}
	return int(s.EndedAt.Sub(s.StartedAt).Minutes())
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// ActiveSession returns the user's currently-running session, if any.
func (r *Repo) ActiveSession(ctx context.Context, userID string) (Session, bool, error) {
	var s Session
	var started int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, started_at, note FROM timer_sessions
		WHERE user_id = ? AND ended_at IS NULL`, userID).
		Scan(&s.ID, &started, &s.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	s.UserID = userID
	s.StartedAt = time.Unix(started, 0)
	return s, true, nil
}

// Start begins a new timer. If one is already running it is returned as-is
// (start is idempotent), so a double-tap never trips the unique index.
func (r *Repo) Start(ctx context.Context, userID string) (Session, error) {
	if s, ok, err := r.ActiveSession(ctx, userID); err != nil {
		return Session{}, err
	} else if ok {
		return s, nil
	}
	now := time.Now()
	s := Session{ID: uuid.NewString(), UserID: userID, StartedAt: now, CreatedAt: now}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO timer_sessions (id, user_id, started_at, ended_at, note, created_at)
		VALUES (?, ?, ?, NULL, '', ?)`,
		s.ID, userID, now.Unix(), now.Unix())
	if err != nil {
		return Session{}, err
	}
	return s, nil
}

// Stop ends the user's running timer and returns the completed session.
// Returns ErrNotFound when no timer is running.
func (r *Repo) Stop(ctx context.Context, userID string) (Session, error) {
	var s Session
	var started int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, started_at, note FROM timer_sessions
		WHERE user_id = ? AND ended_at IS NULL`, userID).
		Scan(&s.ID, &started, &s.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	now := time.Now()
	if _, err := r.DB.ExecContext(ctx, `
		UPDATE timer_sessions SET ended_at = ? WHERE id = ?`, now.Unix(), s.ID); err != nil {
		return Session{}, err
	}
	s.UserID = userID
	s.StartedAt = time.Unix(started, 0)
	ended := now
	s.EndedAt = &ended
	return s, nil
}

// SetNote stores the "what did you do?" answer on a completed session. Scoped
// to the owner.
func (r *Repo) SetNote(ctx context.Context, userID, sessionID, note string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE timer_sessions SET note = ? WHERE id = ? AND user_id = ?`,
		note, sessionID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRecent returns the user's completed sessions, newest first.
func (r *Repo) ListRecent(ctx context.Context, userID string, limit int) ([]Session, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, started_at, ended_at, note FROM timer_sessions
		WHERE user_id = ? AND ended_at IS NOT NULL
		ORDER BY started_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&s.ID, &started, &ended, &s.Note); err != nil {
			return nil, err
		}
		s.UserID = userID
		s.StartedAt = time.Unix(started, 0)
		if ended.Valid {
			e := time.Unix(ended.Int64, 0)
			s.EndedAt = &e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
