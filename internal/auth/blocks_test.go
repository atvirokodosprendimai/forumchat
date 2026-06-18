package auth_test

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// TestUserBlocks_RoundTrip covers the per-viewer mute persistence behind
// the roster menu's Block / Unblock actions.
func TestUserBlocks_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)

	blocker := registerVerified(t, svc, communityID, "ed@example.com")
	target := registerVerified(t, svc, communityID, "fay@example.com")

	// Self-block is a no-op.
	if err := repo.BlockUser(ctx, blocker, blocker, communityID); err != nil {
		t.Fatalf("self-block: %v", err)
	}
	if ids, _ := repo.ListBlocked(ctx, blocker, communityID); len(ids) != 0 {
		t.Fatalf("self-block should not persist, got %v", ids)
	}

	if err := repo.BlockUser(ctx, blocker, target, communityID); err != nil {
		t.Fatalf("block: %v", err)
	}
	// Idempotent.
	if err := repo.BlockUser(ctx, blocker, target, communityID); err != nil {
		t.Fatalf("re-block: %v", err)
	}

	ids, err := repo.ListBlocked(ctx, blocker, communityID)
	if err != nil {
		t.Fatalf("list blocked: %v", err)
	}
	if len(ids) != 1 || ids[0] != target {
		t.Fatalf("want [%s], got %v", target, ids)
	}
	// Block is directional — target hasn't blocked anyone.
	if ids, _ := repo.ListBlocked(ctx, target, communityID); len(ids) != 0 {
		t.Fatalf("block should be one-way, got %v", ids)
	}

	if err := repo.UnblockUser(ctx, blocker, target, communityID); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if ids, _ := repo.ListBlocked(ctx, blocker, communityID); len(ids) != 0 {
		t.Fatalf("after unblock want empty, got %v", ids)
	}
}

// registerVerified registers + verifies a user and returns its user id.
func registerVerified(t *testing.T, svc *auth.Service, communityID, email string) string {
	t.Helper()
	ctx := context.Background()
	code, err := svc.IssueInvite(ctx, communityID, nil, nil)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: email, Password: "supersecret123", InviteCode: code,
	})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify %s: %v", email, err)
	}
	return reg.UserID
}
