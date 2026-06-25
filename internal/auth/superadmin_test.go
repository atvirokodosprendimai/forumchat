package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSuperAdminSet_Has(t *testing.T) {
	s := NewSuperAdminSet([]string{"  Boss@Example.com ", "", "   "})
	if len(s) != 1 {
		t.Fatalf("blank entries must be dropped: got %d entries", len(s))
	}
	if !s.Has("boss@example.com") {
		t.Fatal("expected normalized (trim+lower) match")
	}
	if !s.Has("BOSS@EXAMPLE.COM") {
		t.Fatal("expected case-insensitive match")
	}
	if s.Has("someone@example.com") {
		t.Fatal("unexpected match for non-listed email")
	}

	var empty SuperAdminSet
	if empty.Has("anyone@example.com") {
		t.Fatal("empty/nil set must match nobody")
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func reqWithIdentity(id Identity) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	return r.WithContext(WithIdentity(r.Context(), id))
}

func TestRequireRole_SuperAdminBypassesAdminGate(t *testing.T) {
	// A plain member who is ALSO a platform super-admin must clear an
	// admin-only gate — that's the whole point of god-mode.
	id := Identity{
		User:         User{ID: "u1", Email: "boss@x.com"},
		Membership:   Membership{Role: RoleMember},
		IsSuperAdmin: true,
	}
	rr := httptest.NewRecorder()
	RequireRole(RoleAdmin)(okHandler()).ServeHTTP(rr, reqWithIdentity(id))
	if rr.Code != http.StatusOK {
		t.Fatalf("super-admin should pass admin gate, got %d", rr.Code)
	}
}

func TestRequireRole_NonSuperMemberBlocked(t *testing.T) {
	id := Identity{User: User{ID: "u2"}, Membership: Membership{Role: RoleMember}}
	rr := httptest.NewRecorder()
	RequireRole(RoleAdmin)(okHandler()).ServeHTTP(rr, reqWithIdentity(id))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("plain member must be forbidden from admin gate, got %d", rr.Code)
	}
}

func TestRequireSuperAdmin(t *testing.T) {
	// A community admin who is not on the allowlist must NOT reach /superadmin.
	notSuper := Identity{User: User{ID: "a"}, Membership: Membership{Role: RoleAdmin}}
	rr := httptest.NewRecorder()
	RequireSuperAdmin(okHandler()).ServeHTTP(rr, reqWithIdentity(notSuper))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("community admin must be forbidden from /superadmin, got %d", rr.Code)
	}

	super := Identity{User: User{ID: "b"}, Membership: Membership{Role: RoleMember}, IsSuperAdmin: true}
	rr2 := httptest.NewRecorder()
	RequireSuperAdmin(okHandler()).ServeHTTP(rr2, reqWithIdentity(super))
	if rr2.Code != http.StatusOK {
		t.Fatalf("super-admin must pass /superadmin gate, got %d", rr2.Code)
	}
}

// withSaaSMode sets the package-global SaaSMode for one test and restores it.
// SaaSMode is a boot-time process global, so tests that flip it can't run in
// parallel — they don't call t.Parallel.
func withSaaSMode(t *testing.T, on bool) {
	t.Helper()
	prev := SaaSMode
	SaaSMode = on
	t.Cleanup(func() { SaaSMode = prev })
}

func TestGodMode_SaaSWithdrawsCrossTenantAccess(t *testing.T) {
	super := Identity{User: User{ID: "b"}, IsSuperAdmin: true}
	withSaaSMode(t, false)
	if !super.GodMode() {
		t.Fatal("self-host: a super-admin must have cross-tenant god-mode")
	}
	withSaaSMode(t, true)
	if super.GodMode() {
		t.Fatal("SaaS: a super-admin must NOT have cross-tenant god-mode")
	}
	// A non-super never has god-mode, in either mode.
	plain := Identity{User: User{ID: "m"}, Membership: Membership{Role: RoleAdmin}}
	if plain.GodMode() {
		t.Fatal("a non-super-admin must never have god-mode")
	}
}

func TestRequireRole_SaaSSuperAdminBlockedWithoutRealRole(t *testing.T) {
	// In SaaS a super-admin with only a member role must be refused the admin
	// gate — they can't enter a tenant's /c/<slug>/admin via god-mode.
	withSaaSMode(t, true)
	id := Identity{
		User:         User{ID: "u1", Email: "boss@x.com"},
		Membership:   Membership{Role: RoleMember},
		IsSuperAdmin: true,
	}
	rr := httptest.NewRecorder()
	RequireRole(RoleAdmin)(okHandler()).ServeHTTP(rr, reqWithIdentity(id))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("SaaS super-admin without a real admin role must be forbidden, got %d", rr.Code)
	}
}

func TestRequireSuperAdmin_StillWorksInSaaS(t *testing.T) {
	// The platform-management gate keys on IsSuperAdmin, not GodMode, so it must
	// keep admitting the operator in SaaS (the platform must still work).
	withSaaSMode(t, true)
	super := Identity{User: User{ID: "b"}, Membership: Membership{Role: RoleMember}, IsSuperAdmin: true}
	rr := httptest.NewRecorder()
	RequireSuperAdmin(okHandler()).ServeHTTP(rr, reqWithIdentity(super))
	if rr.Code != http.StatusOK {
		t.Fatalf("SaaS super-admin must still pass the /superadmin gate, got %d", rr.Code)
	}
}

func TestSuperAdminMembership_IsApprovedAdmin(t *testing.T) {
	m := SuperAdminMembership(User{ID: "u", Email: "boss@x.com"}, "c1")
	// Owner so god-mode also clears the owner-only infra gate (SaaS).
	if m.Role != RoleOwner {
		t.Fatalf("synthetic membership must be owner, got %q", m.Role)
	}
	if !m.IsApproved() {
		t.Fatal("synthetic membership must read as approved")
	}
	if m.ID != "" {
		t.Fatal("synthetic membership must have no ID (never persisted)")
	}
	if m.CommunityID != "c1" || m.UserID != "u" {
		t.Fatal("synthetic membership must carry the right community/user")
	}
}
