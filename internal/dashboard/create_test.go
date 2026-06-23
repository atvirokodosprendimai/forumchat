package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func newSaaSHandler(t *testing.T) (*Handler, *auth.Repo, *community.Repo) {
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
	h := &Handler{
		Communities: cRepo,
		Auth:        aRepo,
		Cfg:         config.Config{SAAS: true},
		Provision:   prov,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h, aRepo, cRepo
}

func seedUser(t *testing.T, aRepo *auth.Repo, email string) auth.User {
	t.Helper()
	id := uuid.NewString()
	if _, err := aRepo.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		 VALUES (?, ?, 'h', 'active', 0, 0)`, id, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return auth.User{ID: id, Email: email}
}

func postAs(t *testing.T, h *Handler, fn http.HandlerFunc, u auth.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{User: u}))
	rr := httptest.NewRecorder()
	fn(rr, req)
	return rr
}

// TestPostCreate_FirstIsFree confirms a user owning nothing gets an instant
// community and becomes its owner.
func TestPostCreate_FirstIsFree(t *testing.T) {
	h, aRepo, cRepo := newSaaSHandler(t)
	u := seedUser(t, aRepo, "founder@x.com")

	postAs(t, h, h.PostCreate, u, `{"nc_name":"Alpha","nc_slug":"alpha"}`)

	c, err := cRepo.BySlug(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("community must exist after free create: %v", err)
	}
	owned, _ := aRepo.CountOwnedByUser(context.Background(), u.ID)
	if owned != 1 {
		t.Fatalf("creator must own 1 community, got %d", owned)
	}
	if _, err := chatDefault(t, aRepo, c.ID); err != nil {
		t.Fatalf("new community must have #general: %v", err)
	}
}

// TestPostCreate_SecondBlocked confirms the quota gate is enforced server-side:
// a user who already owns a community cannot create a second via PostCreate.
func TestPostCreate_SecondBlocked(t *testing.T) {
	h, aRepo, cRepo := newSaaSHandler(t)
	u := seedUser(t, aRepo, "founder@x.com")

	postAs(t, h, h.PostCreate, u, `{"nc_name":"Alpha","nc_slug":"alpha"}`)
	postAs(t, h, h.PostCreate, u, `{"nc_name":"Beta","nc_slug":"beta"}`)

	if _, err := cRepo.BySlug(context.Background(), "beta"); err == nil {
		t.Fatalf("second community must NOT be created by a free self-serve create")
	}
	if owned, _ := aRepo.CountOwnedByUser(context.Background(), u.ID); owned != 1 {
		t.Fatalf("owner-count must stay 1 after a blocked second create, got %d", owned)
	}
}

// TestPostRequest_QueuesForApproval confirms an over-quota user's request is
// queued (and a second one is refused while the first is pending).
func TestPostRequest_QueuesForApproval(t *testing.T) {
	h, aRepo, cRepo := newSaaSHandler(t)
	u := seedUser(t, aRepo, "founder@x.com")
	postAs(t, h, h.PostCreate, u, `{"nc_name":"Alpha","nc_slug":"alpha"}`) // now owns one

	postAs(t, h, h.PostRequest, u, `{"cr_name":"Beta","cr_slug":"beta","cr_reason":"team space"}`)
	if n, _ := cRepo.CountPendingRequestsForUser(context.Background(), u.ID); n != 1 {
		t.Fatalf("request must be queued, pending=%d want 1", n)
	}

	postAs(t, h, h.PostRequest, u, `{"cr_name":"Gamma","cr_slug":"gamma","cr_reason":"another"}`)
	if n, _ := cRepo.CountPendingRequestsForUser(context.Background(), u.ID); n != 1 {
		t.Fatalf("a second pending request must be refused, pending=%d want 1", n)
	}
}

func chatDefault(t *testing.T, aRepo *auth.Repo, cid string) (chat.Channel, error) {
	t.Helper()
	return chat.NewRepo(aRepo.DB).DefaultChannel(context.Background(), cid)
}
