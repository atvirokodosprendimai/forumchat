package todos

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// setupRepo opens a migrated temp DB and seeds the community + user that the
// todos FKs require, returning the repo and the seeded ids.
func setupRepo(t *testing.T) (*Repo, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const cid, uid = "c1", "u1"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO communities (id, slug, name, created_at) VALUES (?, 's', 'n', 0)`, cid); err != nil {
		t.Fatalf("seed community: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES (?, 'e@e', 'x', 0, 0)`, uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return NewRepo(db), cid, uid
}

// TestCreateManual verifies a standalone todo (no source) persists — i.e. the
// migration 00048 CHECK accepts source_kind='manual' — and lists with empty
// source fields.
func TestCreateManual(t *testing.T) {
	ctx := context.Background()
	repo, cid, uid := setupRepo(t)

	got, err := repo.Create(ctx, Todo{
		CommunityID: cid,
		UserID:      uid,
		SourceKind:  SourceManual,
		Title:       "buy milk",
		Category:    "errands",
	})
	if err != nil {
		t.Fatalf("create manual: %v", err)
	}
	if got.ID == "" || got.Status != StatusOpen {
		t.Fatalf("unexpected created todo: %+v", got)
	}

	rows, err := repo.ListForUser(ctx, uid, cid, Filter{Status: "active"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.SourceKind != SourceManual || r.SourceID != "" || r.SourceThreadID != "" || r.SourceDay != "" {
		t.Fatalf("manual todo should have empty source fields: %+v", r)
	}
	if r.Title != "buy milk" || r.Category != "errands" {
		t.Fatalf("title/category not persisted: %+v", r)
	}
}
