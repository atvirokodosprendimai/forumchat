package community

import (
	"context"
	"time"
)

// platform_ai.go is the state machine for a community's platform-AI opt-in
// (eidos/spec - saas-platform-ai …). The owner requests; the super-admin grants
// (free) or the Stripe webhook flips the subscription. Authorization is computed
// by PlatformAI in resolve.go: granted-free OR an active subscription.
//
// Every transition loads-then-overlays via Settings/SaveSettings so it never
// wipes the RAG/storage fields it doesn't touch, and so a community with no
// settings row yet gets one created on first request.

// Platform-AI status values (community_settings.platform_ai_status).
const (
	PlatformAIStatusRequested      = "requested"       // owner asked; awaiting super-admin / payment
	PlatformAIStatusApprovedUnpaid = "approved_unpaid" // super-admin approved a paid plan; awaiting checkout
	PlatformAIStatusActive         = "active"          // authorized (granted free OR subscription active)
	PlatformAIStatusCanceled       = "canceled"        // grant removed / subscription lapsed
)

// PlatformAIRequest is one community's platform-AI standing, for the super-admin
// queue + grant controls.
type PlatformAIRequest struct {
	CommunityID string
	Slug        string
	Name        string
	Status      string
	GrantedFree bool
	Subscribed  bool // stripe_subscription_status == "active"
	On          bool // owner master switch
	RequestedAt int64
}

// RequestPlatformAI records the owner's opt-in. If the community is already
// authorized (a prior grant or active subscription) it goes straight to active;
// otherwise it enters the requested queue with a timestamp.
func (r *Repo) RequestPlatformAI(ctx context.Context, communityID string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	on := true
	s.UsePlatformAI = &on
	if boolOr(s.PlatformAIGrantedFree, false) || s.StripeSubscriptionStatus == "active" {
		s.PlatformAIStatus = PlatformAIStatusActive
	} else {
		s.PlatformAIStatus = PlatformAIStatusRequested
		if s.PlatformAIRequestedAt == 0 {
			s.PlatformAIRequestedAt = time.Now().Unix()
		}
	}
	return r.SaveSettings(ctx, s)
}

// CancelPlatformAIRequest withdraws the owner's opt-in without removing any
// grant/subscription state (the owner can re-enable later).
func (r *Repo) CancelPlatformAIRequest(ctx context.Context, communityID string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	off := false
	s.UsePlatformAI = &off
	if !(boolOr(s.PlatformAIGrantedFree, false) || s.StripeSubscriptionStatus == "active") {
		s.PlatformAIStatus = ""
		s.PlatformAIRequestedAt = 0
	}
	return r.SaveSettings(ctx, s)
}

// GrantPlatformAI is the super-admin's free authorization: it sponsors the
// community (no Stripe needed) and marks it active. Implies the master switch on.
func (r *Repo) GrantPlatformAI(ctx context.Context, communityID string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	free, on := true, true
	s.PlatformAIGrantedFree = &free
	s.UsePlatformAI = &on
	s.PlatformAIStatus = PlatformAIStatusActive
	return r.SaveSettings(ctx, s)
}

// RevokePlatformAI removes a free grant and turns the master switch off. A
// community that still holds an active Stripe subscription stays authorized via
// the subscription (the resolver recomputes authorization), so this only revokes
// the SPONSORSHIP, not a paid plan.
func (r *Repo) RevokePlatformAI(ctx context.Context, communityID string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	free := false
	s.PlatformAIGrantedFree = &free
	if s.StripeSubscriptionStatus == "active" {
		s.PlatformAIStatus = PlatformAIStatusActive
	} else {
		off := false
		s.UsePlatformAI = &off
		s.PlatformAIStatus = PlatformAIStatusCanceled
	}
	return r.SaveSettings(ctx, s)
}

// LinkStripeCheckout records a completed Stripe Checkout: the customer +
// subscription ids and the subscription status. An active subscription
// authorizes platform AI and marks it active (the owner had already opted in to
// reach checkout).
func (r *Repo) LinkStripeCheckout(ctx context.Context, communityID, customerID, subscriptionID, status string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	s.StripeCustomerID = customerID
	s.StripeSubscriptionID = subscriptionID
	s.StripeSubscriptionStatus = status
	if SubscriptionGrantsAccess(status) {
		on := true
		s.UsePlatformAI = &on
		s.PlatformAIStatus = PlatformAIStatusActive
	}
	return r.SaveSettings(ctx, s)
}

// MarkStripeEventProcessed records a Stripe event id and reports whether it is
// NEW (true) or a duplicate redelivery already handled (false). The webhook
// skips duplicates, making event handling idempotent against Stripe's
// at-least-once delivery (a replayed checkout.session.completed must not
// re-activate a since-canceled subscription).
func (r *Repo) MarkStripeEventProcessed(ctx context.Context, eventID string) (bool, error) {
	res, err := r.DB.ExecContext(ctx,
		`INSERT OR IGNORE INTO stripe_events (id, created_at) VALUES (?, ?)`,
		eventID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CommunityByStripeCustomer resolves a Stripe customer id back to its community.
// Subscription lifecycle webhooks carry the customer, not our community id.
func (r *Repo) CommunityByStripeCustomer(ctx context.Context, customerID string) (string, error) {
	var cid string
	err := r.DB.QueryRowContext(ctx,
		`SELECT community_id FROM community_settings WHERE stripe_customer_id = ?`, customerID).Scan(&cid)
	return cid, err
}

// SetSubscriptionStatus updates a community's Stripe subscription status from a
// lifecycle webhook and recomputes platform_ai_status: an active subscription
// (or a standing free grant) keeps it active; otherwise it lapses to canceled
// (the resolver then falls the community back to BYO).
func (r *Repo) SetSubscriptionStatus(ctx context.Context, communityID, subscriptionID, status string) error {
	s, err := r.Settings(ctx, communityID)
	if err != nil {
		return err
	}
	// Stale-event guard: ignore a lifecycle event for a subscription that is no
	// longer this community's current one. Stripe can deliver an OLD
	// subscription's deleted/updated event AFTER a newer subscription is active;
	// applying it would wrongly deactivate a live, paying customer.
	if s.StripeSubscriptionID != "" && subscriptionID != "" && s.StripeSubscriptionID != subscriptionID {
		return nil
	}
	if subscriptionID != "" {
		s.StripeSubscriptionID = subscriptionID
	}
	s.StripeSubscriptionStatus = status
	if SubscriptionGrantsAccess(status) || boolOr(s.PlatformAIGrantedFree, false) {
		s.PlatformAIStatus = PlatformAIStatusActive
	} else {
		s.PlatformAIStatus = PlatformAIStatusCanceled
	}
	return r.SaveSettings(ctx, s)
}

// ListPlatformAIRequests returns every community that has engaged the platform-AI
// flow (requested, granted, subscribed, or simply opted-in), newest request
// first, for the super-admin queue. Communities that never touched it are
// excluded.
func (r *Repo) ListPlatformAIRequests(ctx context.Context) ([]PlatformAIRequest, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT cs.community_id, c.slug, c.name,
		       COALESCE(cs.platform_ai_status, ''),
		       COALESCE(cs.platform_ai_granted_free, 0),
		       COALESCE(cs.stripe_subscription_status, ''),
		       COALESCE(cs.use_platform_ai, 0),
		       COALESCE(cs.platform_ai_requested_at, 0)
		FROM community_settings cs
		JOIN communities c ON c.id = cs.community_id
		WHERE COALESCE(cs.use_platform_ai, 0) = 1
		   OR COALESCE(cs.platform_ai_granted_free, 0) = 1
		   OR COALESCE(cs.platform_ai_status, '') != ''
		ORDER BY cs.platform_ai_requested_at DESC, c.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformAIRequest
	for rows.Next() {
		var (
			pr        PlatformAIRequest
			free, on  int
			subStatus string
		)
		if err := rows.Scan(&pr.CommunityID, &pr.Slug, &pr.Name, &pr.Status,
			&free, &subStatus, &on, &pr.RequestedAt); err != nil {
			return nil, err
		}
		pr.GrantedFree = free != 0
		pr.Subscribed = subStatus == "active"
		pr.On = on != 0
		out = append(out, pr)
	}
	return out, rows.Err()
}
