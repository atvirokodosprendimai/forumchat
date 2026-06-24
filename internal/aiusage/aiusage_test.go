package aiusage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// newDB opens a migrated temp SQLite and returns it plus a community repo for
// seeding the FK-required communities rows.
func newDB(t *testing.T) (*sql.DB, *community.Repo) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, &community.Repo{DB: db}
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	// Must not panic on a nil receiver.
	r.Record(context.Background(), Event{CommunityID: "x", Feature: FeatureAgent})
	if ft, err := r.Rollup(context.Background(), "x", 0, 1<<40); err != nil || ft != nil {
		t.Fatalf("nil rollup = %v, %v", ft, err)
	}
}

func TestRecorder_RecordRollupAndTotals(t *testing.T) {
	ctx := context.Background()
	db, crepo := newDB(t)
	rec := New(db, nil)

	a, err := crepo.Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create acme: %v", err)
	}
	b, err := crepo.Create(ctx, "globex", "Globex")
	if err != nil {
		t.Fatalf("create globex: %v", err)
	}

	// Acme: two agent turns + one estimated embed. Globex: one translate.
	rec.Record(ctx, Event{CommunityID: a.ID, Feature: FeatureAgent, Model: "llama", TokensIn: 100, TokensOut: 20})
	rec.Record(ctx, Event{CommunityID: a.ID, Feature: FeatureAgent, Model: "llama", TokensIn: 30, TokensOut: 5})
	rec.Record(ctx, Event{CommunityID: a.ID, Feature: FeatureRAGEmbed, Model: "bge-m3", TokensIn: 12, Estimated: true})
	rec.Record(ctx, Event{CommunityID: b.ID, Feature: FeatureTranslate, Model: "gemma", TokensIn: 8, TokensOut: 24, Estimated: true})

	// Missing mandatory dimensions are dropped, not recorded.
	rec.Record(ctx, Event{Feature: FeatureAgent})
	rec.Record(ctx, Event{CommunityID: a.ID})

	roll, err := rec.Rollup(ctx, a.ID, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 2 {
		t.Fatalf("acme features = %d, want 2: %+v", len(roll), roll)
	}
	// Ordered by feature: "agent" < "rag_embed".
	if roll[0].Feature != FeatureAgent || roll[0].Requests != 2 || roll[0].TokensIn != 130 || roll[0].TokensOut != 25 {
		t.Fatalf("agent rollup wrong: %+v", roll[0])
	}
	if roll[1].Feature != FeatureRAGEmbed || roll[1].Requests != 1 || roll[1].TokensIn != 12 {
		t.Fatalf("embed rollup wrong: %+v", roll[1])
	}

	totals, err := rec.CommunityTotals(ctx, 0, 1<<40)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("community totals = %d, want 2: %+v", len(totals), totals)
	}
	// Busiest first by total tokens: Acme = 130+25+12 = 167 > Globex = 8+24 = 32.
	if totals[0].CommunityID != a.ID || totals[0].Requests != 3 || totals[0].TokensIn != 142 || totals[0].TokensOut != 25 {
		t.Fatalf("acme total wrong: %+v", totals[0])
	}
	if totals[1].CommunityID != b.ID || totals[1].Requests != 1 {
		t.Fatalf("globex total wrong: %+v", totals[1])
	}
}
