package community

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// RequestStatus values for community_requests.status.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
)

// ErrRequestNotFound is returned when a community request id has no row.
var ErrRequestNotFound = errors.New("community: request not found")

// CommunityRequest is one SaaS self-serve request to create an additional
// community, awaiting platform super-admin approval (see migration 00056).
type CommunityRequest struct {
	ID          string
	UserID      string
	Name        string
	Slug        string
	Reason      string
	Status      string
	CommunityID string // set when approved: the provisioned community
	DecidedBy   string
	CreatedAt   time.Time
	DecidedAt   time.Time // zero until decided
}

// PendingRequest is a pending CommunityRequest annotated with the requester's
// email, for the super-admin approval queue.
type PendingRequest struct {
	CommunityRequest
	UserEmail string
}

// CreateRequest files a new pending community-creation request for a user.
func (r *Repo) CreateRequest(ctx context.Context, userID, name, slug, reason string) (CommunityRequest, error) {
	req := CommunityRequest{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      name,
		Slug:      slug,
		Reason:    reason,
		Status:    RequestPending,
		CreatedAt: time.Now(),
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO community_requests (id, user_id, name, slug, reason, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.UserID, req.Name, req.Slug, req.Reason, req.Status, req.CreatedAt.Unix()); err != nil {
		return CommunityRequest{}, err
	}
	return req, nil
}

// CountPendingRequestsForUser reports how many pending requests a user has, so
// the self-serve flow can refuse to queue a second one.
func (r *Repo) CountPendingRequestsForUser(ctx context.Context, userID string) (int, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM community_requests WHERE user_id = ? AND status = ?`,
		userID, RequestPending).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListPendingRequests returns every pending request, oldest first, with the
// requester's email for display. Drives the super-admin approval queue.
func (r *Repo) ListPendingRequests(ctx context.Context) ([]PendingRequest, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT cr.id, cr.user_id, cr.name, cr.slug, cr.reason, cr.status, cr.created_at, u.email
		FROM community_requests cr
		JOIN users u ON u.id = cr.user_id
		WHERE cr.status = ?
		ORDER BY cr.created_at ASC`, RequestPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRequest
	for rows.Next() {
		var p PendingRequest
		var created int64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Slug, &p.Reason, &p.Status, &created, &p.UserEmail); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// RequestByID loads one request. Returns ErrRequestNotFound when absent.
func (r *Repo) RequestByID(ctx context.Context, id string) (CommunityRequest, error) {
	var req CommunityRequest
	var created int64
	var decided sql.NullInt64
	var communityID, decidedBy sql.NullString
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, name, slug, reason, status, community_id, decided_by, created_at, decided_at
		FROM community_requests WHERE id = ?`, id).
		Scan(&req.ID, &req.UserID, &req.Name, &req.Slug, &req.Reason, &req.Status,
			&communityID, &decidedBy, &created, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return CommunityRequest{}, ErrRequestNotFound
	}
	if err != nil {
		return CommunityRequest{}, err
	}
	req.CommunityID = communityID.String
	req.DecidedBy = decidedBy.String
	req.CreatedAt = time.Unix(created, 0)
	if decided.Valid {
		req.DecidedAt = time.Unix(decided.Int64, 0)
	}
	return req, nil
}

// DecideRequest stamps a request as approved or denied. On approve, communityID
// is the provisioned community; on deny it is empty. The UPDATE is guarded on
// status='pending' so a double-submit can't re-decide an already-decided row;
// it returns ErrRequestNotFound when no pending row matched.
func (r *Repo) DecideRequest(ctx context.Context, id, status, deciderID, communityID string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE community_requests
		SET status = ?, decided_by = ?, community_id = ?, decided_at = ?
		WHERE id = ? AND status = ?`,
		status, deciderID, nullIfEmpty(communityID), time.Now().Unix(), id, RequestPending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRequestNotFound
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
