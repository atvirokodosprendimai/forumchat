package admin

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// modFixture is a community with one member per role, ready for hierarchy
// matrix tests against the moderation endpoints.
type modFixture struct {
	h    *Handler
	c    community.Community
	byRo map[auth.Role]auth.Membership // one seeded membership per role
}

// newModFixture seeds a community and one approved member of each role so the
// guard matrix can pick any (actor, target) pair.
func newModFixture(t *testing.T) modFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	aRepo := auth.NewRepo(db)
	cRepo := community.NewRepo(db)
	c, err := cRepo.Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	fx := modFixture{
		h:    &Handler{Repo: aRepo, Log: slog.New(slog.DiscardHandler)},
		c:    c,
		byRo: map[auth.Role]auth.Membership{},
	}
	now := time.Now()
	for _, role := range []auth.Role{auth.RoleMember, auth.RoleMod, auth.RoleAdmin, auth.RoleOwner} {
		uid := seedActiveUser(t, aRepo, string(role)+"@acme.test")
		m := auth.Membership{
			ID: uuid.NewString(), UserID: uid, CommunityID: c.ID,
			DisplayName: string(role), Role: role, ApprovedAt: &now,
		}
		if err := aRepo.CreateMembership(ctx, nil, m); err != nil {
			t.Fatalf("seed membership %s: %v", role, err)
		}
		fx.byRo[role] = m
	}
	return fx
}

// do fires one moderation endpoint as actor against target and returns the
// response code. Identity + community land on the context exactly as the
// Loader/LoadCommunity middlewares would put them.
func (fx modFixture) do(t *testing.T, handler func(http.ResponseWriter, *http.Request), actor, target auth.Role, query string) int {
	t.Helper()
	a := fx.byRo[actor]
	req := httptest.NewRequest(http.MethodPost, "/admin/x?id="+fx.byRo[target].ID+query, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := community.WithContext(req.Context(), fx.c)
	ctx = auth.WithIdentity(ctx, auth.Identity{User: auth.User{ID: a.UserID}, Membership: a})
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))
	return rec.Code
}

// TestModerationHierarchy_StrictOutrank is the matrix behind the reported
// "moderator banned the owner" bug class: every member-moderation endpoint
// must refuse unless the actor STRICTLY outranks the target. The route-level
// RequireRole(RoleAdmin) already stops mods reaching these handlers; this
// matrix pins the handler-level guard so a routing slip can never reopen it.
func TestModerationHierarchy_StrictOutrank(t *testing.T) {
	endpoints := []struct {
		name  string
		call  func(fx modFixture) func(http.ResponseWriter, *http.Request)
		query string
	}{
		{"ban", func(fx modFixture) func(http.ResponseWriter, *http.Request) { return fx.h.PostBan }, ""},
		{"remove", func(fx modFixture) func(http.ResponseWriter, *http.Request) { return fx.h.PostRemoveMember }, ""},
		{"set-role", func(fx modFixture) func(http.ResponseWriter, *http.Request) { return fx.h.PostSetRole }, "&role=member"},
		{"set-nick", func(fx modFixture) func(http.ResponseWriter, *http.Request) { return fx.h.PostSetNick }, ""},
		// Unban is restorative but still hierarchy-bound (Codex finding): an
		// admin must not reverse the owner's moderation call on a peer.
		{"unban", func(fx modFixture) func(http.ResponseWriter, *http.Request) { return fx.h.PostUnban }, ""},
	}
	cases := []struct {
		actor, target auth.Role
		allowed       bool
	}{
		{auth.RoleAdmin, auth.RoleOwner, false}, // F1: admin must never remove/ban the owner
		{auth.RoleAdmin, auth.RoleAdmin, false},
		{auth.RoleAdmin, auth.RoleMod, true},
		{auth.RoleAdmin, auth.RoleMember, true},
		{auth.RoleOwner, auth.RoleAdmin, true}, // owner reins in a rogue admin
		{auth.RoleOwner, auth.RoleOwner, false},
		{auth.RoleMod, auth.RoleOwner, false}, // the reported bug shape (belt-and-braces below RequireRole)
		{auth.RoleMod, auth.RoleAdmin, false},
	}
	for _, ep := range endpoints {
		for _, c := range cases {
			t.Run(ep.name+"/"+string(c.actor)+"-vs-"+string(c.target), func(t *testing.T) {
				fx := newModFixture(t)
				code := fx.do(t, ep.call(fx), c.actor, c.target, ep.query)
				if c.allowed && code >= 400 {
					t.Fatalf("%s: %s acting on %s must be allowed, got %d", ep.name, c.actor, c.target, code)
				}
				if !c.allowed && code < 400 {
					t.Fatalf("%s: %s acting on %s must be refused, got %d", ep.name, c.actor, c.target, code)
				}
			})
		}
	}
}

// TestModerationHierarchy_NoSelfModeration pins the self-guard: even the owner
// cannot ban/remove/demote/alias themselves through the admin endpoints.
func TestModerationHierarchy_NoSelfModeration(t *testing.T) {
	fx := newModFixture(t)
	if code := fx.do(t, fx.h.PostBan, auth.RoleOwner, auth.RoleOwner, ""); code < 400 {
		t.Fatalf("self-ban must be refused, got %d", code)
	}
	if code := fx.do(t, fx.h.PostRemoveMember, auth.RoleAdmin, auth.RoleAdmin, ""); code < 400 {
		t.Fatalf("self-remove must be refused, got %d", code)
	}
}

// TestSetRole_LastAdminGuard pins the role-change orphan guard (Codex
// finding): demoting the community's ONLY admin/owner must be refused —
// otherwise a god-mode operator could strip a community of all privilege.
// The delete path has DeleteMembershipIfNotLastAdmin; this is its role twin.
func TestSetRole_LastAdminGuard(t *testing.T) {
	fx := newModFixture(t)
	ctx := context.Background()
	// Collapse privilege to a single owner: demote the seeded admin first
	// (allowed — the owner still holds privilege).
	if ok, err := fx.h.Repo.UpdateMembershipRoleIfNotLastAdmin(ctx, fx.byRo[auth.RoleAdmin].ID, fx.c.ID, auth.RoleMember); err != nil || !ok {
		t.Fatalf("demoting admin while owner remains must succeed, ok=%v err=%v", ok, err)
	}
	// Now the owner is the last privileged member — the guarded update must refuse.
	if ok, err := fx.h.Repo.UpdateMembershipRoleIfNotLastAdmin(ctx, fx.byRo[auth.RoleOwner].ID, fx.c.ID, auth.RoleMember); err != nil {
		t.Fatalf("guarded update: %v", err)
	} else if ok {
		t.Fatal("demoting the LAST owner must be refused")
	}
	if m, err := fx.h.Repo.MembershipByID(ctx, fx.byRo[auth.RoleOwner].ID); err != nil || m.Role != auth.RoleOwner {
		t.Fatalf("owner role must be untouched, got %v err %v", m.Role, err)
	}
}
