package auth_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// TestJoinReasonRoundTrip confirms a membership's "why do you want to join?"
// note survives CreateMembership and surfaces in the admin pending queue.
func TestJoinReasonRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, communityID := setupSvc(t)

	uid := uuid.NewString()
	if _, err := repo.DB.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		 VALUES (?, 'joiner@example.com', 'h', 'active', 0, 0)`, uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	const reason = "I run a sister community and want to collaborate."
	if err := repo.CreateMembership(ctx, nil, auth.Membership{
		ID:          uuid.NewString(),
		UserID:      uid,
		CommunityID: communityID,
		DisplayName: "joiner",
		Role:        auth.RoleMember,
		JoinReason:  reason,
		// ApprovedAt nil → lands in the pending queue.
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}

	pending, err := repo.ListPendingMemberships(ctx, communityID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending membership, got %d", len(pending))
	}
	if pending[0].JoinReason != reason {
		t.Fatalf("join reason not preserved: got %q want %q", pending[0].JoinReason, reason)
	}
}
