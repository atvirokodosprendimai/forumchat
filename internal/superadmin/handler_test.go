package superadmin

import (
	"context"
	"database/sql"
	"errors"
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

func newTestHandler(t *testing.T) (*Handler, *auth.Repo, *community.Repo) {
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
	return &Handler{AuthRepo: aRepo, Communities: cRepo}, aRepo, cRepo
}

// seedCommunityWithMember creates a community plus one approved member, so a
// delete has dependent rows that ON DELETE CASCADE would erase.
func seedCommunityWithMember(t *testing.T, aRepo *auth.Repo, cRepo *community.Repo) (community.Community, string) {
	t.Helper()
	ctx := context.Background()
	c, err := cRepo.Create(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	uid := uuid.NewString()
	now := time.Now().Unix()
	if _, err := aRepo.DB.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		uid, "m@x.com", "h", "active", now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	approved := time.Now()
	if err := aRepo.CreateMembership(ctx, nil, auth.Membership{
		ID: uuid.NewString(), UserID: uid, CommunityID: c.ID,
		DisplayName: "m", Role: auth.RoleMember, ApprovedAt: &approved,
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	return c, uid
}

func postDelete(h *Handler, cid, confirmSlug string) {
	body := `{"sa_cid":"` + cid + `","sa_confirm_slug":"` + confirmSlug + `"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/community/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostDeleteCommunity(httptest.NewRecorder(), req)
}

func TestPostDeleteCommunity_WrongSlugRefuses(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	c, uid := seedCommunityWithMember(t, aRepo, cRepo)

	postDelete(h, c.ID, "") // empty / non-matching slug must NOT delete

	if _, err := cRepo.ByID(context.Background(), c.ID); err != nil {
		t.Fatalf("community must survive a non-matching slug, got err: %v", err)
	}
	if _, err := aRepo.MembershipFor(context.Background(), uid, c.ID); err != nil {
		t.Fatalf("membership must survive a non-matching slug, got err: %v", err)
	}
}

func TestPostDeleteCommunity_CorrectSlugCascades(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	c, uid := seedCommunityWithMember(t, aRepo, cRepo)

	postDelete(h, c.ID, c.Slug) // exact slug → destructive delete proceeds

	if _, err := cRepo.ByID(context.Background(), c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("community must be deleted on slug match, got err: %v", err)
	}
	// Confirms the cascade is real (and documents it, so nobody re-assumes
	// the FK would "refuse"): the member row is gone too.
	if _, err := aRepo.MembershipFor(context.Background(), uid, c.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("membership must cascade-delete on community delete, got err: %v", err)
	}
}
