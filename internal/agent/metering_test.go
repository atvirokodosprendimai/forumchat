package agent_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// meterSetup returns a migrated DB plus a community id and a real user id (the
// user_id FK on ai_usage_events requires a real row to exercise per-user
// attribution).
func meterSetup(t *testing.T) (db *sql.DB, communityID, userID string) {
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
	userID = "user-1"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES (?,?,?,0,0)`,
		userID, "u@example.com", "x"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return db, c.ID, userID
}

// fakeProvider returns a fixed Usage so the decorator has counts to record.
type fakeProvider struct{ usage agent.Usage }

func (f fakeProvider) Name() string { return "fake" }
func (f fakeProvider) Stream(ctx context.Context, model string, msgs []agent.ChatMessage, tools []agent.ToolDef, onDelta func(string) error) (*agent.StreamResult, error) {
	_ = onDelta("ok")
	return &agent.StreamResult{Usage: f.usage}, nil
}

func TestMeteredProvider_RecordsTurnUsage(t *testing.T) {
	ctx := context.Background()
	db, cid, uid := meterSetup(t)
	rec := aiusage.New(db, nil)

	p := agent.NewMeteredProvider(fakeProvider{usage: agent.Usage{PromptTokens: 50, CompletionTokens: 12}}, rec, cid, uid)
	if _, err := p.Stream(ctx, "llama", nil, nil, func(string) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}

	roll, err := rec.Rollup(ctx, cid, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 1 || roll[0].Feature != aiusage.FeatureAgent {
		t.Fatalf("rollup = %+v, want one agent row", roll)
	}
	if roll[0].Requests != 1 || roll[0].TokensIn != 50 || roll[0].TokensOut != 12 {
		t.Fatalf("agent usage = %+v, want req=1 in=50 out=12", roll[0])
	}
}

func TestMeteredProvider_NilRecorderIsPassthrough(t *testing.T) {
	ctx := context.Background()
	db, cid, _ := meterSetup(t)
	// nil recorder → NewMeteredProvider returns the inner provider unwrapped; no row.
	p := agent.NewMeteredProvider(fakeProvider{usage: agent.Usage{PromptTokens: 9}}, nil, cid, "")
	if _, err := p.Stream(ctx, "llama", nil, nil, func(string) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	roll, err := aiusage.New(db, nil).Rollup(ctx, cid, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 0 {
		t.Fatalf("bare provider must record nothing, got %+v", roll)
	}
}

func TestMeteredTranslate_RecordsEstimated(t *testing.T) {
	ctx := context.Background()
	db, cid, uid := meterSetup(t)
	rec := aiusage.New(db, nil)

	// Minimal Ollama /api/chat stub returning two translation lines.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"content":"Hello there\nHi there"},"done":true,"prompt_eval_count":3,"eval_count":4}`+"\n")
	}))
	defer srv.Close()

	out, err := agent.MeteredTranslate(ctx, rec, cid, uid, srv.URL, "gemma", "labas rytas")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected translations, got none")
	}
	roll, err := rec.Rollup(ctx, cid, 0, 1<<40)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll) != 1 || roll[0].Feature != aiusage.FeatureTranslate || roll[0].Requests != 1 {
		t.Fatalf("translate rollup = %+v, want one translate row", roll)
	}
	// Estimated tokens are derived from text length, so just assert they're > 0.
	if roll[0].TokensIn == 0 || roll[0].TokensOut == 0 {
		t.Fatalf("estimated tokens should be > 0, got %+v", roll[0])
	}
}
