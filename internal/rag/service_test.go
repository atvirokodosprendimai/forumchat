package rag

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// --- fakes ----------------------------------------------------------------

type fakeEmbedder struct{ dim int }

func (e fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, e.dim)
		v[0] = float32(len(texts[i])) // deterministic, length-of-text only
		out[i] = v
	}
	return out, nil
}
func (e fakeEmbedder) Dim() int      { return e.dim }
func (e fakeEmbedder) Model() string { return "fake" }

type fakeStore struct {
	mu   sync.Mutex
	docs map[string]StoredChunk
}

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string]StoredChunk{}} }

func (f *fakeStore) Upsert(ctx context.Context, _, kind, refID string, chunks []StoredChunk) error {
	_ = f.DeleteByRef(ctx, kind, refID)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range chunks {
		f.docs[c.ID] = c
	}
	return nil
}

func (f *fakeStore) DeleteByRef(_ context.Context, kind, refID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := kind + ":" + refID + ":"
	for id := range f.docs {
		if strings.HasPrefix(id, prefix) {
			delete(f.docs, id)
		}
	}
	return nil
}

func (f *fakeStore) Query(_ context.Context, communityID string, _ []float32, limit int) ([]Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Hit
	for _, c := range f.docs {
		if c.Metadata["community_id"] != communityID {
			continue
		}
		out = append(out, Hit{
			Kind: c.Metadata["kind"], RefID: c.Metadata["ref_id"],
			Title: c.Metadata["title"], Snippet: c.Content, Score: 1,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) DropCommunity(_ context.Context, communityID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.docs {
		if c.Metadata["community_id"] == communityID {
			delete(f.docs, id)
		}
	}
	return nil
}

func (f *fakeStore) DropAll(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = map[string]StoredChunk{}
	return nil
}

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) countRef(kind, refID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	prefix := kind + ":" + refID + ":"
	for id := range f.docs {
		if strings.HasPrefix(id, prefix) {
			n++
		}
	}
	return n
}

// --- harness --------------------------------------------------------------

func newTestSvc(t *testing.T) (*Service, *Repo, *fakeStore, func(string, ...any)) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err) // also validates migration 00039 SQL
	}
	repo := NewRepo(db)
	store := newFakeStore()
	svc := NewService(repo, fakeEmbedder{dim: 3}, store, ChunkConfig{BodyTokens: 2800, Overlap: 400}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	return svc, repo, store, exec
}

// drain processes the whole outbox the way Worker.tick does.
func drain(t *testing.T, repo *Repo, svc *Service) {
	t.Helper()
	ctx := context.Background()
	for {
		items, err := repo.Dequeue(ctx, 100)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		if len(items) == 0 {
			return
		}
		for _, it := range items {
			if err := svc.process(ctx, it); err != nil {
				t.Fatalf("process %s/%s: %v", it.Kind, it.RefID, err)
			}
			if err := repo.Ack(ctx, it.Seq); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
	}
}

func seedCommunityUser(exec func(string, ...any), cid, uid string) {
	now := time.Now().Unix()
	exec(`INSERT INTO communities(id, slug, name, created_at) VALUES(?,?,?,?)`, cid, "c-"+cid, "Test", now)
	exec(`INSERT INTO users(id, email, password_hash, status, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		uid, uid+"@x.test", "x", "active", now, now)
}

// --- tests ----------------------------------------------------------------

func TestThreadIndexAndDelete(t *testing.T) {
	svc, repo, store, exec := newTestSvc(t)
	cid, uid, tid := "c1", "u1", "t1"
	seedCommunityUser(exec, cid, uid)
	now := time.Now().Unix()

	// Insert → trigger enqueues 'thread' upsert.
	exec(`INSERT INTO threads(id, community_id, author_id, subject, body_md, body_html, last_activity_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, tid, cid, uid, "Onboarding", "how to set up the project", "", now, now, now)
	drain(t, repo, svc)
	if got := store.countRef(KindThread, tid); got != 1 {
		t.Fatalf("after insert: want 1 chunk, got %d", got)
	}

	// Search finds it.
	hits, err := svc.Search(context.Background(), cid, "set up project", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].RefID != tid {
		t.Fatalf("search: want one hit for %s, got %#v", tid, hits)
	}

	// Soft-delete → trigger enqueues upsert; loader rejects it → vectors removed.
	exec(`UPDATE threads SET deleted_at = ? WHERE id = ?`, now, tid)
	drain(t, repo, svc)
	if got := store.countRef(KindThread, tid); got != 0 {
		t.Fatalf("after soft-delete: want 0 chunks, got %d", got)
	}
}

func TestAIVisibilityGating(t *testing.T) {
	svc, repo, store, exec := newTestSvc(t)
	cid, uid, atid, amid := "c1", "u1", "at1", "am1"
	seedCommunityUser(exec, cid, uid)
	now := time.Now().Unix()

	// Private AI thread + a completed assistant turn.
	exec(`INSERT INTO ai_threads(id, community_id, user_id, visibility, title, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`, atid, cid, uid, "private", "Secret", now, now)
	exec(`INSERT INTO ai_messages(id, thread_id, role, body_md, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`, amid, atid, "assistant", "the secret answer", "done", now, now)
	drain(t, repo, svc)
	if got := store.countRef(KindAI, amid); got != 0 {
		t.Fatalf("private thread must NOT be indexed, got %d chunks", got)
	}

	// Flip to shared → the ai_threads visibility trigger re-enqueues its messages.
	exec(`UPDATE ai_threads SET visibility = 'shared' WHERE id = ?`, atid)
	drain(t, repo, svc)
	if got := store.countRef(KindAI, amid); got != 1 {
		t.Fatalf("shared thread must be indexed, got %d chunks", got)
	}
}

func TestPasteIndexingGating(t *testing.T) {
	svc, repo, store, exec := newTestSvc(t)
	cid, uid, pid := "c1", "u1", "p1"
	seedCommunityUser(exec, cid, uid)
	now := time.Now().Unix()

	// Draft paste (posted_at NULL) — author-private, must NOT be indexed. The
	// INSERT trigger is gated on posted_at IS NOT NULL, so nothing is enqueued.
	exec(`INSERT INTO pastes(id, community_id, author_id, title, language, body, body_html, posted_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,NULL,?,?)`, pid, cid, uid, "Snippet", "go", "func main() { println(\"hi\") }", "", now, now)
	drain(t, repo, svc)
	if got := store.countRef(KindPaste, pid); got != 0 {
		t.Fatalf("draft paste must NOT be indexed, got %d chunks", got)
	}

	// Post it (Save stamps posted_at) — the UPDATE trigger enqueues, the loader
	// now resolves it → indexed and findable.
	exec(`UPDATE pastes SET posted_at = ? WHERE id = ?`, now, pid)
	drain(t, repo, svc)
	if got := store.countRef(KindPaste, pid); got != 1 {
		t.Fatalf("posted paste must be indexed, got %d chunks", got)
	}
	hits, err := svc.Search(context.Background(), cid, "func main println", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].RefID != pid {
		t.Fatalf("search: want one hit for %s, got %#v", pid, hits)
	}

	// Delete → trigger enqueues a delete → vectors removed.
	exec(`DELETE FROM pastes WHERE id = ?`, pid)
	drain(t, repo, svc)
	if got := store.countRef(KindPaste, pid); got != 0 {
		t.Fatalf("after delete: want 0 chunks, got %d", got)
	}
}

func TestReindexCommunity(t *testing.T) {
	svc, repo, store, exec := newTestSvc(t)
	cid, uid := "c1", "u1"
	seedCommunityUser(exec, cid, uid)
	now := time.Now().Unix()
	exec(`INSERT INTO threads(id, community_id, author_id, subject, body_md, body_html, last_activity_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, "t1", cid, uid, "S", "b", "", now, now, now)
	drain(t, repo, svc)
	if store.countRef(KindThread, "t1") != 1 {
		t.Fatal("precondition: thread not indexed")
	}

	// Simulate a dropped store (e.g. backend switch), then reindex re-queues it.
	_ = store.DropAll(context.Background())
	n, err := svc.ReindexCommunity(context.Background(), cid)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if n == 0 {
		t.Fatal("reindex queued nothing")
	}
	drain(t, repo, svc)
	if store.countRef(KindThread, "t1") != 1 {
		t.Fatal("after reindex: thread not re-indexed")
	}
}
