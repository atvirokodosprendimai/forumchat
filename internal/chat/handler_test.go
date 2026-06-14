package chat_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupMentionHandler(t *testing.T) (*chat.Handler, string, auth.Identity) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := community.NewRepo(db)
	c, err := cRepo.BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	aRepo := auth.NewRepo(db)
	// seed two members
	for _, name := range []string{"Alice", "Albert", "Bob"} {
		u := auth.User{ID: uuid.NewString(), Email: strings.ToLower(name) + "@x.test", PasswordHash: "x", Status: auth.StatusActive}
		if err := aRepo.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		m := auth.Membership{ID: uuid.NewString(), UserID: u.ID, CommunityID: c.ID, DisplayName: name, Role: auth.RoleMember}
		if err := aRepo.CreateMembership(ctx, nil, m); err != nil {
			t.Fatalf("create membership: %v", err)
		}
	}
	// caller identity = a fresh user not in the community list, doesn't matter for read-only.
	callerUser := auth.User{ID: uuid.NewString(), Email: "caller@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, callerUser); err != nil {
		t.Fatalf("create caller: %v", err)
	}
	callerMembership := auth.Membership{ID: uuid.NewString(), UserID: callerUser.ID, CommunityID: c.ID, DisplayName: "Caller", Role: auth.RoleMember}
	if err := aRepo.CreateMembership(ctx, nil, callerMembership); err != nil {
		t.Fatalf("create caller membership: %v", err)
	}
	h := &chat.Handler{
		Repo:          chat.NewRepo(db),
		AuthRepo:      aRepo,
		CommunityID:   c.ID,
		CommunityName: c.Name,
		Log:           slog.Default(),
	}
	return h, c.ID, auth.Identity{User: callerUser, Membership: callerMembership}
}

func TestGetMentionSearch_PrefixHits(t *testing.T) {
	t.Parallel()
	h, cid, id := setupMentionHandler(t)

	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22al%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	ctx = auth.WithIdentity(ctx, id)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Alice") {
		t.Errorf("want Alice in SSE body, got:\n%s", body)
	}
	if !strings.Contains(body, "Albert") {
		t.Errorf("want Albert in SSE body, got:\n%s", body)
	}
	if strings.Contains(body, "Bob") {
		t.Errorf("did not want Bob (prefix mismatch), got:\n%s", body)
	}
	if !strings.Contains(body, `id="mention-popup"`) {
		t.Errorf("want #mention-popup root, got:\n%s", body)
	}
}

func TestGetMentionSearch_EmptyQueryEmptyPopup(t *testing.T) {
	t.Parallel()
	h, cid, id := setupMentionHandler(t)

	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	ctx = auth.WithIdentity(ctx, id)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Alice") || strings.Contains(body, "Albert") || strings.Contains(body, "Bob") {
		t.Errorf("expected empty popup for empty query, got:\n%s", body)
	}
}

func TestGetMentionSearch_Unauthed(t *testing.T) {
	t.Parallel()
	h, cid, _ := setupMentionHandler(t)
	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22al%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
