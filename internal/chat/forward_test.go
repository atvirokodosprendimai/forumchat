package chat_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// forwardSetup spins up a migrated tmpdir DB, a bootstrap community, one
// user, and returns the chat service/repo plus those ids.
func forwardSetup(t *testing.T) (*chat.Service, *chat.Repo, string, string) {
	t.Helper()
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
	const uid = "00000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 0, 0)`, uid, "test@example.com", "x"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	repo := chat.NewRepo(db)
	return chat.NewService(repo), repo, c.ID, uid
}

func TestForwardCarriesAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, cid, uid := forwardSetup(t)

	general, err := repo.EnsureDefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	prog, err := svc.CreateChannel(ctx, cid, uid, "Programming", "")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	src, err := svc.Send(ctx, chat.SendInput{
		CommunityID:  cid,
		ChannelID:    general.ID,
		AuthorID:     uid,
		BodyMarkdown: "hello world from general",
	})
	if err != nil {
		t.Fatalf("send source: %v", err)
	}

	fwd, err := svc.Forward(ctx, chat.ForwardInput{
		CommunityID:     cid,
		TargetChannelID: prog.ID,
		AuthorID:        uid,
		Note:            "check this out",
		SourceMsgID:     src.ID,
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if fwd.ChannelID != prog.ID {
		t.Fatalf("forward landed in %q, want target %q", fwd.ChannelID, prog.ID)
	}
	if fwd.ForwardedFromMsgID == nil || *fwd.ForwardedFromMsgID != src.ID {
		t.Fatalf("forwarded_from_msg_id = %v, want %q", fwd.ForwardedFromMsgID, src.ID)
	}
	if fwd.BodyMarkdown != "check this out" {
		t.Fatalf("note body = %q", fwd.BodyMarkdown)
	}

	// Read back through the render path: the forward embed must resolve to
	// the source channel + author + snippet.
	msgs, err := repo.Recent(ctx, prog.ID, 100)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var got *chat.Message
	for i := range msgs {
		if msgs[i].ID == fwd.ID {
			got = &msgs[i]
			break
		}
	}
	if got == nil {
		t.Fatal("forwarded message not found in target channel")
	}
	if got.ForwardedFrom == nil {
		t.Fatal("ForwardedFrom not hydrated on read")
	}
	if got.ForwardedFrom.ChannelSlug != general.Slug {
		t.Errorf("source channel slug = %q, want %q", got.ForwardedFrom.ChannelSlug, general.Slug)
	}
	if !strings.Contains(got.ForwardedFrom.Snippet, "hello world") {
		t.Errorf("snippet = %q, want it to contain source body", got.ForwardedFrom.Snippet)
	}
}

func TestForwardRejectsCrossCommunity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, cid, uid := forwardSetup(t)

	general, err := repo.EnsureDefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	src, err := svc.Send(ctx, chat.SendInput{
		CommunityID:  cid,
		ChannelID:    general.ID,
		AuthorID:     uid,
		BodyMarkdown: "hi",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := svc.Forward(ctx, chat.ForwardInput{
		CommunityID:     "some-other-community",
		TargetChannelID: general.ID,
		AuthorID:        uid,
		SourceMsgID:     src.ID,
	}); err == nil {
		t.Fatal("expected cross-community forward to be rejected")
	}
}
