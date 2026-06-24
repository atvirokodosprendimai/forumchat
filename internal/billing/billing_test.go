package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStore records the calls the webhook makes.
type fakeStore struct {
	linkCommunity, linkCustomer, linkSub, linkStatus string
	linkCalls                                        int
	subCommunity, subID, subStatus                   string
	customerToCommunity                              map[string]string
	seenEvents                                       map[string]bool
}

func (f *fakeStore) MarkStripeEventProcessed(_ context.Context, eventID string) (bool, error) {
	if f.seenEvents == nil {
		f.seenEvents = map[string]bool{}
	}
	if f.seenEvents[eventID] {
		return false, nil
	}
	f.seenEvents[eventID] = true
	return true, nil
}

func (f *fakeStore) LinkStripeCheckout(_ context.Context, communityID, customerID, subscriptionID, status string) error {
	f.linkCommunity, f.linkCustomer, f.linkSub, f.linkStatus = communityID, customerID, subscriptionID, status
	f.linkCalls++
	return nil
}
func (f *fakeStore) CommunityByStripeCustomer(_ context.Context, customerID string) (string, error) {
	if cid, ok := f.customerToCommunity[customerID]; ok {
		return cid, nil
	}
	return "", fmt.Errorf("unknown customer")
}
func (f *fakeStore) SetSubscriptionStatus(_ context.Context, communityID, subscriptionID, status string) error {
	f.subCommunity, f.subID, f.subStatus = communityID, subscriptionID, status
	return nil
}

// signed builds a valid Stripe-Signature header for payload using secret, the
// same scheme webhook.ConstructEvent verifies.
func signed(payload []byte, secret string) string {
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

const secret = "whsec_test_secret"

func post(svc *Service, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/billing/webhook", strings.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	rec := httptest.NewRecorder()
	svc.Webhook(rec, req)
	return rec
}

func TestWebhook_RejectsForgedSignature(t *testing.T) {
	store := &fakeStore{}
	svc := New("", secret, "price_x", "http://x", store, nil)
	body := `{"type":"checkout.session.completed"}`

	// No signature.
	if rec := post(svc, body, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing signature: code = %d, want 400", rec.Code)
	}
	// Signature with the WRONG secret.
	bad := signed([]byte(body), "whsec_attacker")
	if rec := post(svc, body, bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged signature: code = %d, want 400", rec.Code)
	}
	if store.linkCommunity != "" {
		t.Fatal("a rejected webhook must not mutate state")
	}
}

func TestWebhook_CheckoutCompletedLinks(t *testing.T) {
	store := &fakeStore{}
	svc := New("", secret, "price_x", "http://x", store, nil)
	body := `{"id":"evt_co_1","type":"checkout.session.completed","data":{"object":{` +
		`"client_reference_id":"comm-1","customer":{"id":"cus_1"},"subscription":{"id":"sub_1"}}}}`

	rec := post(svc, body, signed([]byte(body), secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid event: code = %d, want 200", rec.Code)
	}
	if store.linkCommunity != "comm-1" || store.linkCustomer != "cus_1" || store.linkSub != "sub_1" || store.linkStatus != "active" {
		t.Fatalf("link not applied: %+v", store)
	}

	// Replay of the SAME event id is a no-op (idempotency gate): still 200, but
	// no second mutation — a redelivered checkout must not re-activate.
	rec = post(svc, body, signed([]byte(body), secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: code = %d, want 200", rec.Code)
	}
	if store.linkCalls != 1 {
		t.Fatalf("replay must not re-apply, link calls = %d, want 1", store.linkCalls)
	}
}

func TestWebhook_StaleSubscriptionEventIgnored(t *testing.T) {
	// A community whose current subscription is sub_new; a late deleted event for
	// the OLD sub_old must be dropped by the store's stale guard. Here we assert
	// the webhook still reaches SetSubscriptionStatus with the event's sub id —
	// the stale guard itself is unit-tested in community.platform_ai_test.go.
	store := &fakeStore{customerToCommunity: map[string]string{"cus_5": "comm-5"}}
	svc := New("", secret, "price_x", "http://x", store, nil)
	body := `{"id":"evt_sub_old","type":"customer.subscription.deleted","data":{"object":{` +
		`"id":"sub_old","status":"canceled","customer":{"id":"cus_5"}}}}`
	rec := post(svc, body, signed([]byte(body), secret))
	if rec.Code != http.StatusOK || store.subID != "sub_old" {
		t.Fatalf("expected dispatch with sub_old, code=%d store=%+v", rec.Code, store)
	}
}

func TestWebhook_SubscriptionDeletedCancels(t *testing.T) {
	store := &fakeStore{customerToCommunity: map[string]string{"cus_9": "comm-9"}}
	svc := New("", secret, "price_x", "http://x", store, nil)
	body := `{"id":"evt_sub_9","type":"customer.subscription.deleted","data":{"object":{` +
		`"id":"sub_9","status":"canceled","customer":{"id":"cus_9"}}}}`

	rec := post(svc, body, signed([]byte(body), secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if store.subCommunity != "comm-9" || store.subID != "sub_9" || store.subStatus != "canceled" {
		t.Fatalf("subscription cancel not applied: %+v", store)
	}
}

func TestEnabled(t *testing.T) {
	if New("", "", "", "", nil, nil).Enabled() {
		t.Fatal("unconfigured billing must be disabled")
	}
	if !New("sk_x", secret, "price_x", "http://x", nil, nil).Enabled() {
		t.Fatal("fully configured billing must be enabled")
	}
	if New("sk_x", "", "price_x", "http://x", nil, nil).Enabled() {
		t.Fatal("missing webhook secret must disable billing")
	}
}
