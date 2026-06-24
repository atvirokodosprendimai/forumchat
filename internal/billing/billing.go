// Package billing is the Stripe integration for paid platform-AI access
// (eidos/spec - saas-platform-ai …). An owner subscribes via a Stripe Checkout
// Session; Stripe's webhook (the SOLE authority on subscription state) drives
// the community's stripe_subscription_status, which the resolver reads to
// authorize platform compute. The app never trusts a client-reported "I paid".
//
// It is inert when unconfigured: Enabled() is false unless the secret key, price
// id and webhook secret are all set, so a deployment without Stripe mounts no
// routes and shows no Subscribe button.
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"
)

// maxWebhookBody caps the webhook payload we read before signature verification.
const maxWebhookBody = 1 << 20

// Store persists Stripe linkage + subscription state. Declared consumer-side so
// billing never imports community (no cycle). Satisfied by *community.Repo.
type Store interface {
	// MarkStripeEventProcessed claims an event id and reports whether it is new
	// (true) or an already-claimed redelivery (false) — the idempotency gate.
	MarkStripeEventProcessed(ctx context.Context, eventID string) (bool, error)
	// UnmarkStripeEvent releases a claimed id after a handling failure so Stripe's
	// retry is not lost.
	UnmarkStripeEvent(ctx context.Context, eventID string) error
	LinkStripeCheckout(ctx context.Context, communityID, customerID, subscriptionID, status string) error
	CommunityByStripeCustomer(ctx context.Context, customerID string) (string, error)
	SetSubscriptionStatus(ctx context.Context, communityID, subscriptionID, status string) error
}

// Service wraps the Stripe SDK for the platform-AI subscription product.
type Service struct {
	secretKey  string
	webhookSec string
	priceID    string
	baseURL    string
	store      Store
	log        *slog.Logger
}

// New configures the Stripe client. Setting stripe.Key is process-global (the
// SDK's design); harmless when empty (Enabled() then reports off).
func New(secretKey, webhookSecret, priceID, baseURL string, store Store, log *slog.Logger) *Service {
	if secretKey != "" {
		stripe.Key = secretKey
	}
	return &Service{
		secretKey:  secretKey,
		webhookSec: webhookSecret,
		priceID:    priceID,
		baseURL:    baseURL,
		store:      store,
		log:        log,
	}
}

// Enabled reports whether Stripe billing is fully configured. All three secrets
// are required: the key (create checkout), the price (what to sell), and the
// webhook secret (verify Stripe's callbacks — without it we'd trust forged
// state, so billing stays off).
func (s *Service) Enabled() bool {
	return s != nil && s.secretKey != "" && s.priceID != "" && s.webhookSec != ""
}

// Checkout creates a subscription Checkout Session for communityID and returns
// the hosted Stripe URL to redirect the owner to. client_reference_id carries
// our community id back on the completed webhook.
func (s *Service) Checkout(ctx context.Context, communityID, slug string) (string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(s.priceID),
			Quantity: stripe.Int64(1),
		}},
		ClientReferenceID: stripe.String(communityID),
		SuccessURL:        stripe.String(s.baseURL + "/c/" + slug + "/settings?billing=success"),
		CancelURL:         stripe.String(s.baseURL + "/c/" + slug + "/settings?billing=cancel"),
	}
	params.Context = ctx
	sess, err := session.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// Webhook is the PUBLIC Stripe webhook endpoint. It verifies the signature with
// the webhook secret (rejecting forged payloads, and replays lacking a valid
// fresh signature), then applies the state change. State updates are idempotent
// (set-to-current), so Stripe's at-least-once redelivery is safe without an
// explicit processed-event table. A handler failure returns 5xx so Stripe
// retries.
func (s *Service) Webhook(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	// Verify the HMAC signature (the security check). Ignore API-version
	// strictness: we read only stable, long-lived fields (client_reference_id,
	// customer/subscription id, status), so a version mismatch between the
	// account's webhook endpoint and the pinned SDK version must not 400 a
	// legitimately-signed event.
	event, err := webhook.ConstructEventWithOptions(payload, r.Header.Get("Stripe-Signature"), s.webhookSec,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		if s.log != nil {
			s.log.Warn("billing: webhook signature rejected", "err", err)
		}
		http.Error(w, "signature", http.StatusBadRequest)
		return
	}
	// Idempotency gate: skip an event id we've already handled (Stripe redelivers
	// at-least-once). A storage error here is transient → 5xx so Stripe retries.
	fresh, err := s.store.MarkStripeEventProcessed(r.Context(), event.ID)
	if err != nil {
		if s.log != nil {
			s.log.Error("billing: dedup", "event", event.ID, "err", err)
		}
		http.Error(w, "dedup", http.StatusInternalServerError)
		return
	}
	if !fresh {
		w.WriteHeader(http.StatusOK) // already processed
		return
	}
	if err := s.handle(r.Context(), event); err != nil {
		// Transient failure: RELEASE the claim so Stripe's retry is reprocessed
		// (otherwise the dedup gate would skip it and the event — e.g. a
		// cancellation — would be lost), then 5xx so Stripe retries. "Not ours /
		// unknown" cases return nil → 200.
		if uerr := s.store.UnmarkStripeEvent(r.Context(), event.ID); uerr != nil && s.log != nil {
			s.log.Error("billing: release event claim", "event", event.ID, "err", uerr)
		}
		if s.log != nil {
			s.log.Error("billing: handle event", "type", event.Type, "err", err)
		}
		http.Error(w, "handle", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handle applies one verified Stripe event. Unknown event types are ignored
// (Stripe sends many we don't subscribe to).
func (s *Service) handle(ctx context.Context, event stripe.Event) error {
	switch event.Type {
	case "checkout.session.completed":
		var cs stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &cs); err != nil {
			return err
		}
		if cs.ClientReferenceID == "" {
			return nil // not initiated by us
		}
		// Link the ids unconditionally so the authoritative subscription lifecycle
		// event can map customer→community. Only AUTHORIZE when the first invoice
		// is actually paid (payment_status=="paid"); a 3DS/incomplete checkout
		// links but does not grant — the customer.subscription.created/updated
		// event flips it to active once Stripe confirms payment.
		status := ""
		if cs.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid {
			status = "active"
		}
		return s.store.LinkStripeCheckout(ctx, cs.ClientReferenceID,
			customerID(cs.Customer), subscriptionID(cs.Subscription), status)

	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			return err
		}
		cust := customerID(sub.Customer)
		if cust == "" {
			return nil
		}
		cid, err := s.store.CommunityByStripeCustomer(ctx, cust)
		if errors.Is(err, sql.ErrNoRows) || cid == "" {
			return nil // unknown / unlinked customer → not ours, nothing to do
		}
		if err != nil {
			return err // transient lookup failure → 5xx → Stripe retries
		}
		return s.store.SetSubscriptionStatus(ctx, cid, sub.ID, string(sub.Status))
	}
	return nil
}

func customerID(c *stripe.Customer) string {
	if c != nil {
		return c.ID
	}
	return ""
}

func subscriptionID(sub *stripe.Subscription) string {
	if sub != nil {
		return sub.ID
	}
	return ""
}
