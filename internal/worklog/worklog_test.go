package worklog_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/worklog"
)

func setup(t *testing.T) (*worklog.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const uid = "00000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 0, 0)`, uid, "test@example.com", "x"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return worklog.NewRepo(db), uid
}

func TestTimerLifecycle(t *testing.T) {
	t.Parallel()
	repo, uid := setup(t)
	ctx := context.Background()

	if _, ok, _ := repo.ActiveSession(ctx, uid); ok {
		t.Fatal("no timer should be active initially")
	}

	s1, err := repo.Start(ctx, uid)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, ok, _ := repo.ActiveSession(ctx, uid); !ok {
		t.Fatal("timer should be active after start")
	}
	// Start is idempotent — a double tap returns the same running session.
	if s1b, err := repo.Start(ctx, uid); err != nil || s1b.ID != s1.ID {
		t.Fatalf("re-start should return same session: id=%s err=%v", s1b.ID, err)
	}

	stopped, err := repo.Stop(ctx, uid)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.ID != s1.ID || stopped.EndedAt == nil {
		t.Fatalf("stopped session wrong: id=%s ended=%v", stopped.ID, stopped.EndedAt)
	}
	if _, ok, _ := repo.ActiveSession(ctx, uid); ok {
		t.Fatal("timer should be inactive after stop")
	}
	// Stopping again with nothing running is ErrNotFound.
	if _, err := repo.Stop(ctx, uid); !errors.Is(err, worklog.ErrNotFound) {
		t.Fatalf("double stop should be ErrNotFound, got %v", err)
	}

	if err := repo.SetNote(ctx, uid, s1.ID, "fixed login bug"); err != nil {
		t.Fatalf("set note: %v", err)
	}

	// A fresh start creates a NEW session (does not resurrect the old one).
	s2, err := repo.Start(ctx, uid)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if s2.ID == s1.ID {
		t.Fatal("second start should mint a new session id")
	}

	recent, err := repo.ListRecent(ctx, uid, 10)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	// Only the completed (s1) session is listed; the running s2 is excluded.
	if len(recent) != 1 {
		t.Fatalf("recent = %d, want 1 (running session excluded)", len(recent))
	}
	if recent[0].Note != "fixed login bug" {
		t.Fatalf("note = %q", recent[0].Note)
	}
}

func TestOneRunningTimerPerUser(t *testing.T) {
	t.Parallel()
	repo, uid := setup(t)
	ctx := context.Background()
	if _, err := repo.Start(ctx, uid); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The partial unique index forbids a second concurrent running row.
	// Insert one directly (bypassing Start's idempotency) and expect failure.
	_, err := repo.DB.ExecContext(ctx, `
		INSERT INTO timer_sessions (id, user_id, started_at, ended_at, note, created_at)
		VALUES ('dupe', ?, 1, NULL, '', 1)`, uid)
	if err == nil {
		t.Fatal("a second running timer should violate uq_timer_active")
	}
}
