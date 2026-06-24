package community

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/config"
)

// platformReady is a SaaS config with the operator's hosted agent configured, so
// PlatformAI authorization is the only variable the transitions move.
func saasCfg() config.Config {
	return config.Config{SAAS: true, AIEnabled: true, PlatformAIAgentBaseURL: "http://p:11434", PlatformAIAgentModel: "glm"}
}

func TestPlatformAI_RequestGrantRevokeLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	cfg := saasCfg()
	c, err := r.Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Owner requests → queued, unauthorized.
	if err := r.RequestPlatformAI(ctx, c.ID); err != nil {
		t.Fatalf("request: %v", err)
	}
	s, _ := r.Settings(ctx, c.ID)
	if s.PlatformAIStatus != PlatformAIStatusRequested || s.PlatformAIRequestedAt == 0 {
		t.Fatalf("after request: %+v", s)
	}
	if on, auth := PlatformAI(s, cfg); !on || auth {
		t.Fatalf("requested must be on but unauthorized: on=%v auth=%v", on, auth)
	}

	// 2. Super-admin grants free → active, authorized, agent now platform.
	if err := r.GrantPlatformAI(ctx, c.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	s, _ = r.Settings(ctx, c.ID)
	if s.PlatformAIStatus != PlatformAIStatusActive {
		t.Fatalf("after grant status = %q", s.PlatformAIStatus)
	}
	if _, auth := PlatformAI(s, cfg); !auth {
		t.Fatal("granted must authorize")
	}
	if !ResolveAgent(s, cfg, false).Platform {
		t.Fatal("granted community must route agents to platform")
	}

	// 3. Super-admin revokes the grant (no subscription) → canceled, BYO again.
	if err := r.RevokePlatformAI(ctx, c.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	s, _ = r.Settings(ctx, c.ID)
	if s.PlatformAIStatus != PlatformAIStatusCanceled {
		t.Fatalf("after revoke status = %q", s.PlatformAIStatus)
	}
	if _, auth := PlatformAI(s, cfg); auth {
		t.Fatal("revoked grant must de-authorize")
	}
	if ResolveAgent(s, cfg, false).Platform {
		t.Fatal("revoked community must fall back to BYO")
	}
}

func TestPlatformAI_RevokeKeepsActiveSubscription(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	cfg := saasCfg()
	c, _ := r.Create(ctx, "globex", "Globex")

	// Owner on platform with an active Stripe subscription (no grant).
	if err := r.SaveSettings(ctx, Settings{
		CommunityID: c.ID, UsePlatformAI: ptrBool(true),
		PlatformAIStatus: PlatformAIStatusActive, StripeSubscriptionStatus: "active",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Revoking the (nonexistent) free grant must NOT cut off a paying customer.
	if err := r.RevokePlatformAI(ctx, c.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	s, _ := r.Settings(ctx, c.ID)
	if _, auth := PlatformAI(s, cfg); !auth {
		t.Fatal("active subscription must stay authorized after grant revoke")
	}
	if s.PlatformAIStatus != PlatformAIStatusActive {
		t.Fatalf("subscribed community must stay active, got %q", s.PlatformAIStatus)
	}
}

func TestSetSubscriptionStatus_StaleEventIgnored(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	cfg := saasCfg()
	c, _ := r.Create(ctx, "acme", "Acme")

	// Current subscription sub_new is active.
	if err := r.SaveSettings(ctx, Settings{
		CommunityID: c.ID, UsePlatformAI: ptrBool(true),
		StripeCustomerID: "cus_1", StripeSubscriptionID: "sub_new",
		StripeSubscriptionStatus: "active", PlatformAIStatus: PlatformAIStatusActive,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A late "canceled" for the OLD subscription must NOT deactivate the live one.
	if err := r.SetSubscriptionStatus(ctx, c.ID, "sub_old", "canceled"); err != nil {
		t.Fatalf("stale event: %v", err)
	}
	s, _ := r.Settings(ctx, c.ID)
	if _, auth := PlatformAI(s, cfg); !auth || s.StripeSubscriptionStatus != "active" {
		t.Fatalf("stale old-sub event must not deactivate live sub: %+v", s)
	}

	// The current subscription's own cancellation IS applied.
	if err := r.SetSubscriptionStatus(ctx, c.ID, "sub_new", "canceled"); err != nil {
		t.Fatalf("current event: %v", err)
	}
	s, _ = r.Settings(ctx, c.ID)
	if _, auth := PlatformAI(s, cfg); auth {
		t.Fatal("current-sub cancellation must deactivate")
	}
}

func TestRevokePlatformAI_KeepsTrialingSubscriber(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	cfg := saasCfg()
	c, _ := r.Create(ctx, "trialco", "Trial Co")

	// A trialing subscriber who ALSO holds a free grant.
	if err := r.SaveSettings(ctx, Settings{
		CommunityID: c.ID, UsePlatformAI: ptrBool(true), PlatformAIGrantedFree: ptrBool(true),
		StripeSubscriptionStatus: "trialing", PlatformAIStatus: PlatformAIStatusActive,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Revoking the free grant must NOT cut off a trialing (paying) customer —
	// trialing grants access just like active.
	if err := r.RevokePlatformAI(ctx, c.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	s, _ := r.Settings(ctx, c.ID)
	if _, auth := PlatformAI(s, cfg); !auth || s.PlatformAIStatus != PlatformAIStatusActive {
		t.Fatalf("trialing subscriber must stay authorized after grant revoke: %+v", s)
	}
}

func TestSubscriptionGrantsAccess(t *testing.T) {
	for _, st := range []string{"active", "trialing"} {
		if !SubscriptionGrantsAccess(st) {
			t.Fatalf("%q should grant", st)
		}
	}
	for _, st := range []string{"", "past_due", "canceled", "unpaid", "incomplete", "paused"} {
		if SubscriptionGrantsAccess(st) {
			t.Fatalf("%q should NOT grant", st)
		}
	}
}

func TestListPlatformAIRequests(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	a, _ := r.Create(ctx, "acme", "Acme")
	b, _ := r.Create(ctx, "globex", "Globex")
	_, _ = r.Create(ctx, "initech", "Initech") // never touches platform AI → excluded

	if err := r.RequestPlatformAI(ctx, a.ID); err != nil {
		t.Fatalf("request a: %v", err)
	}
	if err := r.GrantPlatformAI(ctx, b.ID); err != nil {
		t.Fatalf("grant b: %v", err)
	}

	list, err := r.ListPlatformAIRequests(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 engaged communities, got %d: %+v", len(list), list)
	}
	byID := map[string]PlatformAIRequest{}
	for _, pr := range list {
		byID[pr.CommunityID] = pr
	}
	if byID[a.ID].Status != PlatformAIStatusRequested || byID[a.ID].GrantedFree {
		t.Fatalf("acme should be requested-not-granted: %+v", byID[a.ID])
	}
	if byID[b.ID].Status != PlatformAIStatusActive || !byID[b.ID].GrantedFree {
		t.Fatalf("globex should be granted-active: %+v", byID[b.ID])
	}
}
