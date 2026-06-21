package webhooks_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/webhooks"
)

// TestInboundVertical exercises the full inbound path at the data layer:
// create an inbound webhook, look it up by token, post a bot message into its
// channel via the chat service, and read it back as a KindWebhook message that
// carries the webhook's bot identity.
func TestInboundVertical(t *testing.T) {
	t.Parallel()
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
	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	general, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}

	whRepo := webhooks.NewRepo(db)
	whSvc := webhooks.NewService(whRepo)
	wh, err := whSvc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID,
		Direction:   webhooks.DirIn,
		Provider:    "github",
		Name:        "GitHub",
		AvatarURL:   "https://example.com/gh.png",
		ChannelID:   general.ID,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if wh.Token == "" {
		t.Fatal("inbound webhook should mint a token")
	}

	got, err := whRepo.InboundByToken(ctx, wh.Token)
	if err != nil {
		t.Fatalf("InboundByToken: %v", err)
	}
	if got.ChannelID != general.ID || got.Name != "GitHub" {
		t.Fatalf("InboundByToken mismatch: %+v", got)
	}

	if _, err := chatSvc.PostBot(ctx, c.ID, general.ID, got.Name, got.AvatarURL, "**alice** pushed 1 commit"); err != nil {
		t.Fatalf("PostBot: %v", err)
	}

	msgs, err := chatRepo.Recent(ctx, general.ID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Kind != chat.KindWebhook {
		t.Fatalf("kind = %q, want webhook", m.Kind)
	}
	if m.AuthorID != nil {
		t.Fatalf("bot message must have no author_id, got %v", *m.AuthorID)
	}
	if m.BotName != "GitHub" || m.AuthorName != "GitHub" {
		t.Fatalf("bot name not carried: BotName=%q AuthorName=%q", m.BotName, m.AuthorName)
	}
	if m.BotAvatar != "https://example.com/gh.png" {
		t.Fatalf("bot avatar not carried: %q", m.BotAvatar)
	}
}

// TestInboundByTokenMiss confirms a bad / disabled token is not found.
func TestInboundByTokenMiss(t *testing.T) {
	t.Parallel()
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
	repo := webhooks.NewRepo(db)
	if _, err := repo.InboundByToken(ctx, "does-not-exist"); err != webhooks.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := repo.InboundByToken(ctx, ""); err != webhooks.ErrNotFound {
		t.Fatalf("empty token want ErrNotFound, got %v", err)
	}
}

// TestOutboundCreateValidation checks provider×direction and URL validation.
func TestOutboundCreateValidation(t *testing.T) {
	t.Parallel()
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
	svc := webhooks.NewService(webhooks.NewRepo(db))

	// github is inbound-only → rejected for outbound.
	if _, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "github",
		Name: "x", TargetURL: "https://example.com",
	}); err != webhooks.ErrBadProvider {
		t.Fatalf("want ErrBadProvider, got %v", err)
	}

	// outbound without a valid URL → rejected.
	if _, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "slack",
		Name: "x", TargetURL: "not-a-url",
	}); err != webhooks.ErrTargetURL {
		t.Fatalf("want ErrTargetURL, got %v", err)
	}

	// valid outbound slack webhook → ok, no token.
	wh, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "slack",
		Name: "Slack relay", TargetURL: "https://hooks.slack.com/services/x",
	})
	if err != nil {
		t.Fatalf("valid outbound create: %v", err)
	}
	if wh.Token != "" {
		t.Fatal("outbound webhook must not have a token")
	}
}
