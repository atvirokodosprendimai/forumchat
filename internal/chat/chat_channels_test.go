package chat_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// chanTestEnv opens a migrated temp DB, bootstraps a community, seeds its
// #general + one user (for the created_by FK), and returns the repo +
// service + community id + that user id.
func chanTestEnv(t *testing.T) (*chat.Repo, *chat.Service, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
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
	u := auth.User{ID: uuid.NewString(), Email: "creator@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	repo := chat.NewRepo(db)
	if _, err := repo.EnsureDefaultChannel(ctx, c.ID); err != nil {
		t.Fatalf("default channel: %v", err)
	}
	return repo, chat.NewService(repo), c.ID, u.ID
}

func TestCreateChannel_SlugCapReserved(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := chanTestEnv(t)
	ctx := context.Background()

	// happy path: name → slug
	ch, err := svc.CreateChannel(ctx, cid, uid, "Design Team", "ui talk")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ch.Slug != "design-team" {
		t.Fatalf("want slug design-team, got %q", ch.Slug)
	}

	// reserved
	if _, err := svc.CreateChannel(ctx, cid, uid, "general", ""); !errors.Is(err, chat.ErrReservedSlug) {
		t.Fatalf("want ErrReservedSlug, got %v", err)
	}

	// duplicate slug
	if _, err := svc.CreateChannel(ctx, cid, uid, "Design Team", ""); !errors.Is(err, chat.ErrSlugTaken) {
		t.Fatalf("want ErrSlugTaken, got %v", err)
	}

	// empty name
	if _, err := svc.CreateChannel(ctx, cid, uid, "   ", ""); !errors.Is(err, chat.ErrEmptyChannelName) {
		t.Fatalf("want ErrEmptyChannelName, got %v", err)
	}

	// cap: #general + design-team already = 2 non-archived; create up to the cap.
	existing, _ := repo.ListChannels(ctx, cid, false)
	for i := len(existing); i < chat.MaxChannelsPerCommunity; i++ {
		if _, err := svc.CreateChannel(ctx, cid, uid, "Channel "+uuid.NewString()[:6], ""); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := svc.CreateChannel(ctx, cid, uid, "One Too Many", ""); !errors.Is(err, chat.ErrChannelCap) {
		t.Fatalf("want ErrChannelCap at limit, got %v", err)
	}
}

func TestDefaultChannelGuard(t *testing.T) {
	t.Parallel()
	repo, svc, cid, _ := chanTestEnv(t)
	ctx := context.Background()

	gen, err := repo.DefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if err := svc.Archive(ctx, cid, gen.ID); !errors.Is(err, chat.ErrDefaultChannel) {
		t.Fatalf("archive default: want ErrDefaultChannel, got %v", err)
	}
	if err := svc.Delete(ctx, cid, gen.ID); !errors.Is(err, chat.ErrDefaultChannel) {
		t.Fatalf("delete default: want ErrDefaultChannel, got %v", err)
	}
	if _, err := svc.RenameChannel(ctx, cid, gen.ID, "lobby"); !errors.Is(err, chat.ErrDefaultChannel) {
		t.Fatalf("rename default: want ErrDefaultChannel, got %v", err)
	}
}

func TestArchiveHidesFromSwitcher(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := chanTestEnv(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, cid, uid, "temp", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Archive(ctx, cid, ch.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	active, _ := repo.ListChannels(ctx, cid, false)
	for _, c := range active {
		if c.ID == ch.ID {
			t.Fatalf("archived channel still in non-archived list")
		}
	}
	all, _ := repo.ListChannels(ctx, cid, true)
	found := false
	for _, c := range all {
		if c.ID == ch.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("archived channel missing from includeArchived list")
	}
}

func TestUnreadChannels(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := chanTestEnv(t)
	ctx := context.Background()

	gen, _ := repo.DefaultChannel(ctx, cid)
	design, err := svc.CreateChannel(ctx, cid, uid, "design", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A message in #design, none read yet → #design is unread for viewer.
	if err := repo.Insert(ctx, chat.Message{
		ID: uuid.NewString(), CommunityID: cid, ChannelID: design.ID,
		Kind: chat.KindUser, BodyMarkdown: "hi", BodyHTML: "hi", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	unread, err := repo.UnreadChannels(ctx, cid, "viewer")
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if !unread[design.ID] {
		t.Fatalf("want #design unread, got %v", unread)
	}
	if unread[gen.ID] {
		t.Fatalf("empty #general should not be unread")
	}

	// After marking #design read past the message, the dot clears.
	if err := repo.MarkRead(ctx, "viewer", cid, design.ID, "", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("markread: %v", err)
	}
	unread, _ = repo.UnreadChannels(ctx, cid, "viewer")
	if unread[design.ID] {
		t.Fatalf("want #design read after MarkRead, got unread")
	}
}
