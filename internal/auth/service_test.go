package auth_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupSvc(t *testing.T) (*auth.Service, *auth.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := community.NewRepo(db)
	c, err := cRepo.BootstrapOrFetch(ctx, "test", "Test Community")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	repo := auth.NewRepo(db)
	svc := &auth.Service{
		Repo:      repo,
		Mailer:    &auth.LogMailer{Log: slog.Default()},
		BaseURL:   "http://test",
		VerifyTTL: time.Hour,
		InviteTTL: time.Hour,
	}
	return svc, repo, c.ID
}

func TestRegisterVerifyLogin_Happy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, communityID := setupSvc(t)

	code, err := svc.IssueInvite(ctx, communityID, nil, nil)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}

	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "alice@example.com", Password: "supersecret123", InviteCode: code,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.VerificationToken == "" {
		t.Fatal("expected verification token")
	}

	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify: %v", err)
	}

	res, err := svc.Login(ctx, "alice@example.com", "supersecret123", communityID)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.User.Status != auth.StatusActive {
		t.Fatalf("expected active status, got %s", res.User.Status)
	}
	if res.Membership.Role != auth.RoleMember {
		t.Fatalf("expected member role, got %s", res.Membership.Role)
	}
}

func TestRegister_InvalidInvite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, _ := setupSvc(t)

	_, err := svc.Register(ctx, auth.RegisterInput{
		Email: "x@example.com", Password: "supersecret123", InviteCode: "NOSUCHCODE",
	})
	if err == nil {
		t.Fatal("expected error for bad invite")
	}
}

// Unlimited invites (max_uses=nil) accept multiple registrants. Capped
// invites reject once uses_count == max_uses.
func TestInviteCaps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, communityID := setupSvc(t)

	// Unlimited: two registrations on the same code both succeed.
	unlimited, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "u1@example.com", Password: "supersecret123", InviteCode: unlimited,
	}); err != nil {
		t.Fatalf("first unlimited register: %v", err)
	}
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "u2@example.com", Password: "supersecret123", InviteCode: unlimited,
	}); err != nil {
		t.Fatalf("second unlimited register: %v", err)
	}

	// max_uses=1 behaves like the old single-use code.
	one := 1
	capped, _ := svc.IssueInvite(ctx, communityID, nil, &one)
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "c1@example.com", Password: "supersecret123", InviteCode: capped,
	}); err != nil {
		t.Fatalf("first capped register: %v", err)
	}
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "c2@example.com", Password: "supersecret123", InviteCode: capped,
	}); err == nil {
		t.Fatal("expected error for exhausted invite")
	}
}

func TestLogin_NotVerified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, communityID := setupSvc(t)

	code, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "pending@example.com", Password: "supersecret123", InviteCode: code,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Login(ctx, "pending@example.com", "supersecret123", communityID); err == nil {
		t.Fatal("expected error for unverified login")
	}
}

func TestLogin_BadPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, communityID := setupSvc(t)

	code, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	reg, _ := svc.Register(ctx, auth.RegisterInput{
		Email: "p@example.com", Password: "supersecret123", InviteCode: code,
	})
	_, _ = svc.Verify(ctx, reg.VerificationToken, communityID)
	if _, err := svc.Login(ctx, "p@example.com", "wrongpassword", communityID); err == nil {
		t.Fatal("expected error for bad password")
	}
}
