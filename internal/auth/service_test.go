package auth_test

import (
	"context"
	"errors"
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
		Repo:        repo,
		Mailer:      &auth.LogMailer{Log: slog.Default()},
		BaseURL:     "http://test",
		VerifyTTL:   time.Hour,
		InviteTTL:   time.Hour,
		CommunityID: c.ID,
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

// Open registration off (default): registering without an invite is refused.
func TestRegister_ClosedNoInvite_Refused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, _ := setupSvc(t)

	_, err := svc.Register(ctx, auth.RegisterInput{
		Email: "stranger@example.com", Password: "supersecret123",
	})
	if !errors.Is(err, auth.ErrInviteRequired) {
		t.Fatalf("want ErrInviteRequired, got %v", err)
	}
}

// Open registration on, auto-approve off: a no-invite registrant verifies and
// lands in the pending queue (approved_at = NULL).
func TestRegister_OpenNoInvite_PendingQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	svc.OpenRegistration = true

	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "open1@example.com", Password: "supersecret123",
	})
	if err != nil {
		t.Fatalf("open register: %v", err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	if m.ApprovedAt != nil {
		t.Fatalf("want pending (approved_at nil), got approved at %v", m.ApprovedAt)
	}
}

// Open registration on AND auto-approve on: a no-invite registrant is approved
// at verify time and skips the queue.
func TestRegister_OpenNoInvite_AutoApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	svc.OpenRegistration = true
	svc.OpenRegistrationAutoApprove = true

	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "open2@example.com", Password: "supersecret123",
	})
	if err != nil {
		t.Fatalf("open register: %v", err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	if m.ApprovedAt == nil {
		t.Fatal("want auto-approved (approved_at set), got nil")
	}
}

// Auto-approve is independent of open registration: an invite-based signup
// with auto-approve on (open reg off) is also approved at verify time.
func TestRegister_AutoApprove_InviteFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	svc.OpenRegistrationAutoApprove = true // OpenRegistration stays false

	code, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "invited-auto@example.com", Password: "supersecret123", InviteCode: code,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	if m.ApprovedAt == nil {
		t.Fatal("want auto-approved (approved_at set) for invite flow, got nil")
	}
}

// AutoVerifyEmail skips the email round-trip: the user is active + a member
// right after Register (no Verify call) and can log in immediately.
func TestRegister_AutoVerifyEmail_LogsInWithoutEmail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	svc.OpenRegistration = true
	svc.OpenRegistrationAutoApprove = true
	svc.AutoVerifyEmail = true

	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "demo@example.com", Password: "supersecret123",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !reg.AutoVerified {
		t.Fatal("want AutoVerified=true")
	}
	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership should exist immediately after auto-verify: %v", err)
	}
	if m.ApprovedAt == nil {
		t.Fatal("want approved (auto-approve on)")
	}
	if _, err := svc.Login(ctx, "demo@example.com", "supersecret123", communityID); err != nil {
		t.Fatalf("login straight after auto-verify (no email click): %v", err)
	}
}

// AutoVerifyEmail is independent of auto-approve: email is skipped but, without
// auto-approve, the verified member still lands in the pending queue.
func TestRegister_AutoVerifyEmail_StillQueuesWithoutAutoApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	svc.OpenRegistration = true
	svc.AutoVerifyEmail = true // OpenRegistrationAutoApprove stays false

	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "demo2@example.com", Password: "supersecret123",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !reg.AutoVerified {
		t.Fatal("want AutoVerified=true")
	}
	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership should exist: %v", err)
	}
	if m.ApprovedAt != nil {
		t.Fatal("want pending (auto-approve off), got approved")
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

func TestMagicLink_IssueAndConsume_ActivatesPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)

	code, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "magic@example.com", Password: "supersecret123", InviteCode: code,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.IssueMagicLink(ctx, "magic@example.com"); err != nil {
		t.Fatalf("issue magic: %v", err)
	}
	// pull the freshest magic_login token directly from the repo (mailer is a log noop in tests)
	var token string
	if err := repo.DB.QueryRowContext(ctx,
		`SELECT token FROM verification_tokens WHERE purpose='magic_login' ORDER BY expires_at DESC LIMIT 1`).
		Scan(&token); err != nil {
		t.Fatalf("read token: %v", err)
	}
	res, err := svc.ConsumeMagicLink(ctx, token, communityID)
	if err != nil {
		t.Fatalf("consume magic: %v", err)
	}
	if res.User.Status != auth.StatusActive {
		t.Fatalf("want StatusActive after consume, got %s", res.User.Status)
	}
	if res.Membership.CommunityID != communityID {
		t.Fatalf("want membership in community, got %q", res.Membership.CommunityID)
	}
}

func TestMagicLink_UnknownEmail_NoError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, _ := setupSvc(t)
	if err := svc.IssueMagicLink(ctx, "nobody@example.com"); err != nil {
		t.Fatalf("unknown email should be silent no-op, got: %v", err)
	}
}

func TestMagicLink_InvalidToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, communityID := setupSvc(t)
	if _, err := svc.ConsumeMagicLink(ctx, "not-a-real-token", communityID); err == nil {
		t.Fatal("want error for invalid token")
	}
}

func TestMagicLink_SecondConsumeFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	code, _ := svc.IssueInvite(ctx, communityID, nil, nil)
	reg, _ := svc.Register(ctx, auth.RegisterInput{
		Email: "once@example.com", Password: "supersecret123", InviteCode: code,
	})
	_, _ = svc.Verify(ctx, reg.VerificationToken, communityID)
	if err := svc.IssueMagicLink(ctx, "once@example.com"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var token string
	_ = repo.DB.QueryRowContext(ctx,
		`SELECT token FROM verification_tokens WHERE purpose='magic_login' ORDER BY expires_at DESC LIMIT 1`).
		Scan(&token)
	if _, err := svc.ConsumeMagicLink(ctx, token, communityID); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := svc.ConsumeMagicLink(ctx, token, communityID); err == nil {
		t.Fatal("want error on re-use")
	}
}
