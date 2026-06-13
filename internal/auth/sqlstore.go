package auth

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// SQLStore implements alexedwards/scs/v2.Store on top of the project's
// modernc.org/sqlite handle. We can't use scs's bundled sqlite3store because
// it depends on CGO mattn/go-sqlite3, which conflicts with our CGO-free
// modernc driver.
//
// Sessions survive process restart this way (memstore would lose them).
type SQLStore struct {
	DB      *sql.DB
	cleanup sync.Once
}

// NewSQLStore returns a Store and kicks off a background goroutine that
// expires stale tokens once a minute. Pass the same *sql.DB used by every
// other repo.
//
// The constructor self-heals: it runs CREATE TABLE IF NOT EXISTS so an app
// that boots against an old DB (e.g. MIGRATE_ON_BOOT=false or the user
// hadn't restarted to pick up migration 00002 yet) doesn't crash with
// "no such table: sessions" on the next request.
func NewSQLStore(ctx context.Context, db *sql.DB) *SQLStore {
	s := &SQLStore{DB: db}
	_, _ = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			token  TEXT PRIMARY KEY,
			data   BLOB NOT NULL,
			expiry INTEGER NOT NULL
		)`)
	_, _ = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expiry)`)
	s.cleanup.Do(func() {
		go s.gc(ctx)
	})
	return s
}

func (s *SQLStore) gc(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expiry < ?`, time.Now().Unix())
		}
	}
}

// Find satisfies scs.Store.
func (s *SQLStore) Find(token string) ([]byte, bool, error) {
	var data []byte
	var exp int64
	err := s.DB.QueryRowContext(context.Background(),
		`SELECT data, expiry FROM sessions WHERE token = ?`, token).
		Scan(&data, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if time.Now().Unix() > exp {
		return nil, false, nil
	}
	return data, true, nil
}

// Commit satisfies scs.Store.
func (s *SQLStore) Commit(token string, b []byte, expiry time.Time) error {
	_, err := s.DB.ExecContext(context.Background(), `
		INSERT INTO sessions (token, data, expiry) VALUES (?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET data = excluded.data, expiry = excluded.expiry`,
		token, b, expiry.Unix())
	return err
}

// Delete satisfies scs.Store.
func (s *SQLStore) Delete(token string) error {
	_, err := s.DB.ExecContext(context.Background(), `DELETE FROM sessions WHERE token = ?`, token)
	return err
}
