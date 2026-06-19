package timebudget_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/timebudget"
)

func setup(t *testing.T) (*timebudget.Repo, string, string) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	const uid = "00000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 0, 0)`, uid, "test@example.com", "x"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return timebudget.NewRepo(db), c.ID, uid
}

func TestBudgetUpsert(t *testing.T) {
	t.Parallel()
	repo, cid, uid := setup(t)
	ctx := context.Background()

	b, err := repo.GetBudget(ctx, cid)
	if err != nil {
		t.Fatalf("get empty budget: %v", err)
	}
	if b.MonthlyMinutes != 0 {
		t.Fatalf("unset budget should be 0, got %d", b.MonthlyMinutes)
	}

	if err := repo.SetBudget(ctx, cid, 3000, uid); err != nil { // 50h
		t.Fatalf("set budget: %v", err)
	}
	if b, _ = repo.GetBudget(ctx, cid); b.MonthlyMinutes != 3000 {
		t.Fatalf("budget = %d, want 3000", b.MonthlyMinutes)
	}
	// Upsert replaces, never duplicates (community_id is PK).
	if err := repo.SetBudget(ctx, cid, 1800, uid); err != nil {
		t.Fatalf("re-set budget: %v", err)
	}
	if b, _ = repo.GetBudget(ctx, cid); b.MonthlyMinutes != 1800 {
		t.Fatalf("budget after upsert = %d, want 1800", b.MonthlyMinutes)
	}
}

func TestEntriesMonthlyBuckets(t *testing.T) {
	t.Parallel()
	repo, cid, uid := setup(t)
	ctx := context.Background()

	mk := func(min int, day string) timebudget.Entry {
		e, err := repo.AddEntry(ctx, timebudget.Entry{
			CommunityID: cid, Minutes: min, Note: "work " + day, OccurredOn: day, CreatedBy: uid,
		})
		if err != nil {
			t.Fatalf("add entry %s: %v", day, err)
		}
		return e
	}
	mk(120, "2026-06-10")
	e2 := mk(30, "2026-06-11")
	mk(60, "2026-05-15") // different month

	if used, _ := repo.UsedMinutes(ctx, cid, "2026-06"); used != 150 {
		t.Fatalf("June used = %d, want 150", used)
	}
	if used, _ := repo.UsedMinutes(ctx, cid, "2026-05"); used != 60 {
		t.Fatalf("May used = %d, want 60", used)
	}

	rows, err := repo.ListEntries(ctx, cid, "2026-06")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("June entries = %d, want 2", len(rows))
	}

	// A non-owner without mod rights cannot delete someone else's entry.
	if err := repo.DeleteEntry(ctx, cid, e2.ID, "someone-else", false); !errors.Is(err, timebudget.ErrNotFound) {
		t.Fatalf("scoped delete should be ErrNotFound, got %v", err)
	}
	// Mod (all=true) can.
	if err := repo.DeleteEntry(ctx, cid, e2.ID, "mod-user", true); err != nil {
		t.Fatalf("mod delete: %v", err)
	}
	if used, _ := repo.UsedMinutes(ctx, cid, "2026-06"); used != 120 {
		t.Fatalf("June used after delete = %d, want 120", used)
	}
}
