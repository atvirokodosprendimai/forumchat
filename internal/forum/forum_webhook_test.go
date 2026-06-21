package forum_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// TestWebhookThreadAndPost covers the inbound Matrix-thread path: the first
// message opens a forum thread, a later message appends a bot-authored post,
// and the far-side author name overrides the sentinel identity on render.
func TestWebhookThreadAndPost(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(ctx, dbPath)
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

	repo := forum.NewRepo(db)
	svc := forum.NewService(repo, time.Minute)

	// First inbound message opens a thread (no explicit subject → derived).
	th, err := svc.CreateWebhookThread(ctx, c.ID, "alice", "", "Hello from Matrix")
	if err != nil {
		t.Fatalf("CreateWebhookThread: %v", err)
	}
	if th.Subject != "Hello from Matrix" {
		t.Fatalf("subject = %q", th.Subject)
	}
	if !strings.Contains(th.BodyMarkdown, "alice") {
		t.Fatalf("body should attribute alice: %q", th.BodyMarkdown)
	}
	if th.AuthorID != forum.AgentBotUserID {
		t.Fatalf("thread author = %q, want sentinel", th.AuthorID)
	}

	// A later message appends a post carrying the far-side author identity.
	if _, err := svc.CreateWebhookPost(ctx, th.ID, "bob", "", "a reply"); err != nil {
		t.Fatalf("CreateWebhookPost: %v", err)
	}
	posts, err := repo.ListPosts(ctx, th.ID)
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	p := posts[0]
	if p.AuthorName != "bob" {
		t.Fatalf("post author = %q, want bob (bot_name override)", p.AuthorName)
	}
	if p.IsBot() {
		t.Fatal("webhook post must not be an AI bot post (no agent_id)")
	}
}
