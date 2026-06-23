package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// newCreateHandler builds the minimal admin.Handler that PostCreateCommunity
// needs: the auth repo (user lookup) and the shared provisioner.
func newCreateHandler(t *testing.T) (*Handler, *auth.Repo, *community.Repo, *chat.Repo) {
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
	chatRepo := chat.NewRepo(db)
	prov := &provision.Service{
		Communities: cRepo,
		Auth:        aRepo,
		SeedChannel: func(ctx context.Context, cid string) error {
			_, err := chatRepo.EnsureDefaultChannel(ctx, cid)
			return err
		},
	}
	return &Handler{Repo: aRepo, Provision: prov}, aRepo, cRepo, chatRepo
}

func seedActiveUser(t *testing.T, aRepo *auth.Repo, email string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := aRepo.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		 VALUES (?, ?, 'h', 'active', 0, 0)`, id, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func postCreate(h *Handler, body string) {
	req := httptest.NewRequest(http.MethodPost, "/admin/create-community", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostCreateCommunity(httptest.NewRecorder(), req)
}

// TestAdminCreateCommunity_SeedsOwnerAndChannel locks the per-community admin
// create path after it was refactored onto provision.Service: the community is
// created, its first member becomes OWNER, and #general is seeded.
func TestAdminCreateCommunity_SeedsOwnerAndChannel(t *testing.T) {
	h, aRepo, cRepo, chatRepo := newCreateHandler(t)
	ctx := context.Background()
	uid := seedActiveUser(t, aRepo, "alice@acme.com")

	postCreate(h, `{"cc_name":"Acme","cc_slug":"acme","cc_member_email":"alice@acme.com"}`)

	c, err := cRepo.BySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("community must be created: %v", err)
	}
	if owned, _ := aRepo.CountOwnedByUser(ctx, uid); owned != 1 {
		t.Fatalf("first member must be OWNER, owned=%d want 1", owned)
	}
	if ch, err := chatRepo.DefaultChannel(ctx, c.ID); err != nil || ch.Slug != "general" {
		t.Fatalf("new community must have #general, got ch=%+v err=%v", ch, err)
	}
}

// TestAdminCreateCommunity_DuplicateSlug confirms a taken slug is rejected and
// no second community / membership leaks through.
func TestAdminCreateCommunity_DuplicateSlug(t *testing.T) {
	h, aRepo, cRepo, _ := newCreateHandler(t)
	ctx := context.Background()
	seedActiveUser(t, aRepo, "alice@acme.com")
	uid2 := seedActiveUser(t, aRepo, "bob@acme.com")

	postCreate(h, `{"cc_name":"Acme","cc_slug":"acme","cc_member_email":"alice@acme.com"}`)
	// Second create reusing the slug, different member, must be refused.
	postCreate(h, `{"cc_name":"Acme Two","cc_slug":"acme","cc_member_email":"bob@acme.com"}`)

	c, err := cRepo.BySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("original community must still exist: %v", err)
	}
	if c.Name != "Acme" {
		t.Fatalf("slug must still map to the FIRST community, got name=%q", c.Name)
	}
	if owned, _ := aRepo.CountOwnedByUser(ctx, uid2); owned != 0 {
		t.Fatalf("rejected duplicate must not seed a membership for bob, owned=%d want 0", owned)
	}
}

// TestAdminCreateCommunity_UnknownEmail confirms a non-existent member email is
// rejected without creating a community.
func TestAdminCreateCommunity_UnknownEmail(t *testing.T) {
	h, _, cRepo, _ := newCreateHandler(t)
	ctx := context.Background()

	postCreate(h, `{"cc_name":"Ghost","cc_slug":"ghost","cc_member_email":"nobody@nowhere.com"}`)

	if _, err := cRepo.BySlug(ctx, "ghost"); err == nil {
		t.Fatalf("community must NOT be created for an unknown member email")
	}
}
