package rag_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/rag"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func meterSetup(t *testing.T) (*sql.DB, string) {
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
	c, err := (&community.Repo{DB: db}).Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	return db, c.ID
}

// fakeEmbedder returns dim-zero vectors; we only care that Embed is observed.
type fakeEmbedder struct{ model string }

func (f fakeEmbedder) Dim() int      { return 3 }
func (f fakeEmbedder) Model() string { return f.model }
func (f fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0, 0, 0}
	}
	return out, nil
}

func TestMeteredEmbedder_RecordsEstimatedTokens(t *testing.T) {
	ctx := context.Background()
	db, cid := meterSetup(t)
	rec := aiusage.New(db, nil)

	e := rag.NewMeteredEmbedder(fakeEmbedder{model: "bge-m3"}, rec, cid)
	if _, err := e.Embed(ctx, []string{"hello world", "another chunk of text here"}); err != nil {
		t.Fatalf("embed: %v", err)
	}

	roll, err := rec.Rollup(ctx, cid, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 1 || roll[0].Feature != aiusage.FeatureRAGEmbed {
		t.Fatalf("rollup = %+v, want one rag_embed row", roll)
	}
	if roll[0].Requests != 1 || roll[0].TokensIn == 0 {
		t.Fatalf("embed usage = %+v, want req=1 in>0", roll[0])
	}
}

func TestMeteredEmbedder_NilRecorderIsPassthrough(t *testing.T) {
	ctx := context.Background()
	db, cid := meterSetup(t)
	e := rag.NewMeteredEmbedder(fakeEmbedder{model: "bge-m3"}, nil, cid)
	if _, err := e.Embed(ctx, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	roll, err := aiusage.New(db, nil).Rollup(ctx, cid, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 0 {
		t.Fatalf("bare embedder must record nothing, got %+v", roll)
	}
}
