package uploads

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// TestOrphanQueryMatchesSchema runs the sweep's orphan query against a freshly
// migrated DB so a wrong table or column name (e.g. a body-bearing table that
// lacks body_html) is caught here instead of silently breaking the live sweep
// at runtime — the query error is only logged, never surfaced.
func TestOrphanQueryMatchesSchema(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := db.QueryContext(ctx, orphanQuery(), int64(0))
	if err != nil {
		t.Fatalf("orphan query does not match schema: %v", err)
	}
	rows.Close()
}
