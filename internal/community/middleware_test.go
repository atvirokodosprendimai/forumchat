package community

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// newAuthRepo builds an auth.Repo over a fresh migrated sqlite DB plus a single
// community to resolve against. It returns the repo and the community.
func newAuthRepo(t *testing.T) (*auth.Repo, Community) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := &Repo{DB: db}
	c, err := cRepo.Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	return &auth.Repo{DB: db}, c
}

// reqFor builds a request carrying the community + identity in context, the way
// LoadCommunity + auth.Loader would have set them upstream of RequireMember.
func reqFor(c Community, id auth.Identity) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/c/"+c.Slug+"/chat", nil)
	ctx := WithContext(r.Context(), c)
	ctx = auth.WithIdentity(ctx, id)
	return r.WithContext(ctx)
}

// TestRequireMember_SuperAdminGodModeGatedBySaaS is the privacy-wall regression:
// a super-admin with NO real membership may enter a community's content routes
// only in self-host (god-mode), and must be refused in SaaS.
func TestRequireMember_SuperAdminGodModeGatedBySaaS(t *testing.T) {
	authRepo, c := newAuthRepo(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// The super-admin holds no membership row in this community.
	super := auth.Identity{User: auth.User{ID: "boss"}, IsSuperAdmin: true}

	// Self-host: god-mode synthesizes a membership → admitted.
	prev := auth.SaaSMode
	t.Cleanup(func() { auth.SaaSMode = prev })

	auth.SaaSMode = false
	rr := httptest.NewRecorder()
	RequireMember(authRepo)(ok).ServeHTTP(rr, reqFor(c, super))
	if rr.Code != http.StatusOK {
		t.Fatalf("self-host: super-admin must reach content via god-mode, got %d", rr.Code)
	}

	// SaaS: god-mode withdrawn → a non-member operator is forbidden, exactly
	// like any other non-member. This is the leak that this change closes.
	auth.SaaSMode = true
	rr2 := httptest.NewRecorder()
	RequireMember(authRepo)(ok).ServeHTTP(rr2, reqFor(c, super))
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("SaaS: non-member super-admin must be forbidden, got %d", rr2.Code)
	}
}
