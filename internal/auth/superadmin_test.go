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

func TestSuperAdminMembership_IsApprovedAdmin(t *testing.T) {
	m := SuperAdminMembership(User{ID: "u", Email: "boss@x.com"}, "c1")
	if m.Role != RoleAdmin {
		t.Fatalf("synthetic membership must be admin, got %q", m.Role)
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
