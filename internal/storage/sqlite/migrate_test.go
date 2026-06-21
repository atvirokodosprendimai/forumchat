package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestBackfillDefaultChannel proves migration 00052 heals a community that was
// created at runtime without a #general channel: migrate to 00051, insert a
// channel-less community (the exact state the super-admin/admin create flow
// left behind before the handlers were fixed), then migrate to 00052 and
// assert it gained a default #general. This is the fix for the first chat
// visit crashing with "load channel: sql: no rows in result set".
func TestBackfillDefaultChannel(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 51); err != nil {
		t.Fatalf("migrate to 51: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO communities (id, slug, name, created_at) VALUES ('c1','runtime','Runtime',0)`); err != nil {
		t.Fatalf("insert community: %v", err)
	}
	var before int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM chat_channels WHERE community_id='c1'`).Scan(&before); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if before != 0 {
		t.Fatalf("precondition: runtime community must start with no channels, got %d", before)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 52); err != nil {
		t.Fatalf("migrate to 52: %v", err)
	}

	var slug string
	var isDefault int
	if err := db.QueryRowContext(ctx,
		`SELECT slug, is_default FROM chat_channels WHERE community_id='c1'`).Scan(&slug, &isDefault); err != nil {
		t.Fatalf("backfill must create a default channel: %v", err)
	}
	if slug != "general" || isDefault != 1 {
		t.Fatalf("backfilled channel = (slug=%q, is_default=%d), want (general, 1)", slug, isDefault)
	}
}
