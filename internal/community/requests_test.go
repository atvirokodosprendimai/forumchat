package community

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// seedUser inserts a minimal users row so community_requests.user_id satisfies
// its FK. Returns the new user id.
func seedUser(t *testing.T, r *Repo, email string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := r.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', 0, 0)`, id, email, "h"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func TestCommunityRequests_Lifecycle(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	uid := seedUser(t, r, "founder@x.com")

	// No pending requests initially.
	if n, err := r.CountPendingRequestsForUser(ctx, uid); err != nil || n != 0 {
		t.Fatalf("initial pending = %d, err %v; want 0", n, err)
	}

	req, err := r.CreateRequest(ctx, uid, "Beta", "beta", "need a second space")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if req.Status != RequestPending {
		t.Fatalf("new request status = %q, want %q", req.Status, RequestPending)
	}

	if n, _ := r.CountPendingRequestsForUser(ctx, uid); n != 1 {
		t.Fatalf("pending after create = %d, want 1", n)
	}

	pend, err := r.ListPendingRequests(ctx)
	if err != nil || len(pend) != 1 {
		t.Fatalf("list pending = %d (err %v), want 1", len(pend), err)
	}
	if pend[0].UserEmail != "founder@x.com" || pend[0].Slug != "beta" {
		t.Fatalf("pending row = %+v, want email/slug of the request", pend[0])
	}

	// Approve: stamps community_id + status, clears the pending count.
	cid := uuid.NewString()
	if err := r.DecideRequest(ctx, req.ID, RequestApproved, "super-1", cid); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := r.RequestByID(ctx, req.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got.Status != RequestApproved || got.CommunityID != cid || got.DecidedBy != "super-1" {
		t.Fatalf("approved request = %+v, want approved/cid/decider stamped", got)
	}
	if got.DecidedAt.IsZero() {
		t.Fatalf("approved request must have decided_at set")
	}
	if n, _ := r.CountPendingRequestsForUser(ctx, uid); n != 0 {
		t.Fatalf("pending after approve = %d, want 0", n)
	}

	// Deciding an already-decided request is a no-op guarded by status=pending.
	if err := r.DecideRequest(ctx, req.ID, RequestDenied, "super-2", ""); err != ErrRequestNotFound {
		t.Fatalf("re-decide should return ErrRequestNotFound, got %v", err)
	}
}

func TestCountOwnedByUser_viaRequestsDB(t *testing.T) {
	// Sanity: CountPendingRequestsForUser returns 0 for an unknown user (no panic,
	// no FK issues on a read). The owner-count gate itself lives in auth.Repo and
	// is covered there; this guards the community side reads against an empty DB.
	ctx := context.Background()
	r := newTestRepo(t)
	if n, err := r.CountPendingRequestsForUser(ctx, "nobody"); err != nil || n != 0 {
		t.Fatalf("unknown user pending = %d, err %v; want 0", n, err)
	}
	if pend, err := r.ListPendingRequests(ctx); err != nil || len(pend) != 0 {
		t.Fatalf("empty pending = %d, err %v; want 0", len(pend), err)
	}
}
