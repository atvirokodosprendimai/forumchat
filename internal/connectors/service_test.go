package connectors_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/connectors"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// setup boots a migrated temp DB with a bootstrap community + #general channel
// and returns the pieces a connector test needs.
func setup(t *testing.T) (context.Context, *connectors.Service, *connectors.Repo, *auth.Repo, community.Community, chat.Channel) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	general, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	authSvc := &auth.Service{Repo: auth.NewRepo(db)}
	connRepo := connectors.NewRepo(db)
	connSvc := connectors.NewService(connRepo, authSvc, chatRepo)
	return ctx, connSvc, connRepo, authSvc.Repo, c, general
}

func TestServiceCreateProvisionsMember(t *testing.T) {
	t.Parallel()
	ctx, svc, repo, authRepo, c, general := setup(t)

	conn, err := svc.Create(ctx, connectors.CreateInput{
		CommunityID:  c.ID,
		Name:         "Acme Support",
		ChannelIDs:   []string{general.ID, "bogus-foreign-channel"},
		Capabilities: []string{"send", "delete", "fly"}, // "fly" is unknown → dropped
		MentionsOnly: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Secret minted, member provisioned.
	if len(conn.Secret) < 40 {
		t.Fatalf("secret too short: %q", conn.Secret)
	}
	if conn.UserID == "" {
		t.Fatal("no synthetic member user id")
	}

	// The synthetic member is a real, approved membership named after the connector.
	m, err := authRepo.MembershipFor(ctx, conn.UserID, c.ID)
	if err != nil {
		t.Fatalf("membership lookup: %v", err)
	}
	if m.DisplayName != "Acme Support" || m.Role != auth.RoleMember {
		t.Fatalf("unexpected membership: %+v", m)
	}
	if m.ApprovedAt == nil {
		t.Fatal("service member not auto-approved")
	}

	// Only the valid channel was persisted; the forged one was dropped.
	chs, err := repo.Channels(ctx, conn.ID)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if len(chs) != 1 || chs[0] != general.ID {
		t.Fatalf("channel allowlist = %v, want [%s]", chs, general.ID)
	}

	// Unknown capability dropped; granted ones kept.
	stored, err := repo.ByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("byID: %v", err)
	}
	if !stored.Can(connectors.CapSend) || !stored.Can(connectors.CapDelete) || stored.Can("fly") {
		t.Fatalf("capabilities = %v", stored.Capabilities)
	}
}

func TestServiceRotateInvalidatesOldSig(t *testing.T) {
	t.Parallel()
	ctx, svc, _, _, c, _ := setup(t)

	conn, err := svc.Create(ctx, connectors.CreateInput{CommunityID: c.ID, Name: "bot", Capabilities: []string{"send"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	oldSig := connectors.StreamSig(conn.Secret, conn.ID, 0)

	newSecret, err := svc.Rotate(ctx, c.ID, conn.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newSecret == conn.Secret {
		t.Fatal("rotate did not change the secret")
	}
	// The URL minted under the old secret no longer verifies.
	if connectors.VerifyStream(newSecret, conn.ID, oldSig, 0, time.Now()) {
		t.Fatal("old stream signature still valid after rotate")
	}
}

func TestServiceDeleteRemovesMember(t *testing.T) {
	t.Parallel()
	ctx, svc, repo, authRepo, c, _ := setup(t)

	conn, err := svc.Create(ctx, connectors.CreateInput{CommunityID: c.ID, Name: "bot", Capabilities: []string{"send"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, c.ID, conn.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.ByID(ctx, conn.ID); err == nil {
		t.Fatal("connector still present after delete")
	}
	if _, err := authRepo.MembershipFor(ctx, conn.UserID, c.ID); err == nil {
		t.Fatal("synthetic membership survived connector delete")
	}
}
