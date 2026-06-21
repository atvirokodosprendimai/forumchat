package superadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
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

// fakeBroadcaster records each SystemBroadcast call so a test can assert the
// platform broadcast fanned out to every community with the rendered body.
type fakeBroadcaster struct {
	cids  []string
	htmls []string
}

func (f *fakeBroadcaster) SystemBroadcast(_ context.Context, cid, html string) error {
	f.cids = append(f.cids, cid)
	f.htmls = append(f.htmls, html)
	return nil
}

func postBroadcast(h *Handler, message string) {
	body := `{"sa_broadcast":` + jsonString(message) + `}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/broadcast", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostBroadcast(httptest.NewRecorder(), req)
}

// jsonString quotes s as a JSON string literal for the request body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestPostBroadcast_FansToAllCommunities is the feature core: one super-admin
// announcement lands in EVERY community's #general, rendered through markdown
// and carrying the platform banner.
func TestPostBroadcast_FansToAllCommunities(t *testing.T) {
	h, _, cRepo := newTestHandler(t)
	ctx := context.Background()
	a, err := cRepo.Create(ctx, "alpha", "Alpha")
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	b, err := cRepo.Create(ctx, "bravo", "Bravo")
	if err != nil {
		t.Fatalf("create bravo: %v", err)
	}
	fb := &fakeBroadcaster{}
	h.Chat = fb

	postBroadcast(h, "hello **world**")

	if len(fb.cids) != 2 {
		t.Fatalf("want a broadcast per community (2), got %d", len(fb.cids))
	}
	got := map[string]bool{fb.cids[0]: true, fb.cids[1]: true}
	if !got[a.ID] || !got[b.ID] {
		t.Fatalf("both communities must receive the broadcast, got %v", fb.cids)
	}
	html := fb.htmls[0]
	if !strings.Contains(html, "Platform broadcast") {
		t.Errorf("broadcast must carry the platform banner, got %q", html)
	}
	if !strings.Contains(html, "<strong>world</strong>") {
		t.Errorf("broadcast body must be rendered markdown, got %q", html)
	}
}

// TestPostBroadcast_EmptyMessageNoOp guards against blasting a blank message to
// every community.
func TestPostBroadcast_EmptyMessageNoOp(t *testing.T) {
	h, _, cRepo := newTestHandler(t)
	if _, err := cRepo.Create(context.Background(), "alpha", "Alpha"); err != nil {
		t.Fatalf("create: %v", err)
	}
	fb := &fakeBroadcaster{}
	h.Chat = fb

	postBroadcast(h, "   ")

	if len(fb.cids) != 0 {
		t.Fatalf("blank message must broadcast to nobody, got %d calls", len(fb.cids))
	}
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

// insertUser inserts a bare active user and returns its id.
func insertUser(t *testing.T, aRepo *auth.Repo, email string) string {
	t.Helper()
	uid := uuid.NewString()
	now := time.Now().Unix()
	if _, err := aRepo.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		uid, email, "h", "active", now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return uid
}

// joinCommunity makes uid an approved member of cid with the given role.
func joinCommunity(t *testing.T, aRepo *auth.Repo, uid, cid string, role auth.Role) string {
	t.Helper()
	approved := time.Now()
	mid := uuid.NewString()
	if err := aRepo.CreateMembership(context.Background(), nil, auth.Membership{
		ID: mid, UserID: uid, CommunityID: cid, DisplayName: "m", Role: role, ApprovedAt: &approved,
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	return mid
}

// insertThread inserts one non-deleted thread authored by uid in cid.
func insertThread(t *testing.T, aRepo *auth.Repo, uid, cid string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := aRepo.DB.ExecContext(context.Background(),
		`INSERT INTO threads (id, community_id, author_id, subject, body_md, body_html, last_activity_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		uuid.NewString(), cid, uid, "s", "b", "<p>b</p>", now, now, now); err != nil {
		t.Fatalf("insert thread: %v", err)
	}
}

func userStatus(t *testing.T, aRepo *auth.Repo, uid string) string {
	t.Helper()
	var s string
	if err := aRepo.DB.QueryRowContext(context.Background(),
		`SELECT status FROM users WHERE id = ?`, uid).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

// TestUserMembershipsDrillDown is the core of the reported bug: the platform
// roster shows a bare community COUNT with no way to see which. UserMemberships
// is the query that answers "which 2?", with per-community role + activity.
func TestUserMembershipsDrillDown(t *testing.T) {
	_, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	cb, _ := cRepo.Create(ctx, "bravo", "Bravo")
	uid := insertUser(t, aRepo, "u@x.com")
	joinCommunity(t, aRepo, uid, ca.ID, auth.RoleMember)
	joinCommunity(t, aRepo, uid, cb.ID, auth.RoleAdmin)
	insertThread(t, aRepo, uid, cb.ID) // activity only in Bravo

	mems, err := aRepo.UserMemberships(ctx, uid)
	if err != nil {
		t.Fatalf("UserMemberships: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("want 2 communities, got %d", len(mems))
	}
	// Ordered by community name → Alpha first, Bravo second.
	if mems[0].Slug != "alpha" || mems[1].Slug != "bravo" {
		t.Fatalf("unexpected order: %s, %s", mems[0].Slug, mems[1].Slug)
	}
	if mems[0].ThreadCount != 0 || mems[1].ThreadCount != 1 {
		t.Fatalf("thread counts wrong: alpha=%d bravo=%d", mems[0].ThreadCount, mems[1].ThreadCount)
	}
	if mems[1].Role != auth.RoleAdmin || mems[0].MembershipID == "" {
		t.Fatalf("role/membership-id not surfaced: %+v", mems)
	}
}

// TestPostSystemBan_DisablesAndWipes verifies the platform kill switch:
// account disabled + all content soft-deleted across every community.
func TestPostSystemBan_DisablesAndWipes(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	cb, _ := cRepo.Create(ctx, "bravo", "Bravo")
	uid := insertUser(t, aRepo, "u@x.com")
	joinCommunity(t, aRepo, uid, ca.ID, auth.RoleMember)
	joinCommunity(t, aRepo, uid, cb.ID, auth.RoleMember)
	insertThread(t, aRepo, uid, ca.ID)
	insertThread(t, aRepo, uid, cb.ID)

	body := `{"sa_uid":"` + uid + `"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/user/sysban", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostSystemBan(httptest.NewRecorder(), req)

	if got := userStatus(t, aRepo, uid); got != string(auth.StatusDisabled) {
		t.Fatalf("account must be disabled, got %q", got)
	}
	mems, err := aRepo.UserMemberships(ctx, uid)
	if err != nil {
		t.Fatalf("UserMemberships: %v", err)
	}
	for _, m := range mems {
		if m.ThreadCount != 0 {
			t.Fatalf("threads must be wiped in %s, got %d", m.Slug, m.ThreadCount)
		}
	}
}

// TestPostSystemBan_SelfRefuses guards against a super-admin nuking themselves.
func TestPostSystemBan_SelfRefuses(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	uid := insertUser(t, aRepo, "boss@x.com")
	joinCommunity(t, aRepo, uid, ca.ID, auth.RoleAdmin)

	body := `{"sa_uid":"` + uid + `"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/user/sysban", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(),
		auth.Identity{User: auth.User{ID: uid}, IsSuperAdmin: true}))
	h.PostSystemBan(httptest.NewRecorder(), req)

	if got := userStatus(t, aRepo, uid); got != string(auth.StatusActive) {
		t.Fatalf("self system-ban must be refused, status=%q", got)
	}
}

// TestPostCommunityRemove_LastAdminRefused keeps a community from being
// orphaned by removing its sole admin from the platform page.
func TestPostCommunityRemove_LastAdminRefused(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	uid := insertUser(t, aRepo, "solo@x.com")
	mid := joinCommunity(t, aRepo, uid, ca.ID, auth.RoleAdmin)

	body := `{"sa_mid":"` + mid + `","sa_uid":"` + uid + `"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/user/community/remove", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostCommunityRemove(httptest.NewRecorder(), req)

	if _, err := aRepo.MembershipByID(ctx, mid); err != nil {
		t.Fatalf("last-admin membership must survive removal, got err: %v", err)
	}
}

func postRole(h *Handler, mid, uid, role string) {
	body := `{"sa_mid":"` + mid + `","sa_uid":"` + uid + `","sa_role":"` + role + `"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/user/community/role", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostCommunityRole(httptest.NewRecorder(), req)
}

// TestPostCommunityRole_PromotesToAdmin is the requested feature: a super-admin
// makes an existing member a community admin from the platform drill-down.
func TestPostCommunityRole_PromotesToAdmin(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	uid := insertUser(t, aRepo, "u@x.com")
	mid := joinCommunity(t, aRepo, uid, ca.ID, auth.RoleMember)

	postRole(h, mid, uid, string(auth.RoleAdmin))

	got, err := aRepo.MembershipByID(ctx, mid)
	if err != nil {
		t.Fatalf("membership by id: %v", err)
	}
	if got.Role != auth.RoleAdmin {
		t.Fatalf("want role=admin after promote, got %s", got.Role)
	}
}

// TestPostCommunityRole_LastAdminDemoteRefused keeps a community from losing
// its sole admin via the role switcher (mirrors the remove guard).
func TestPostCommunityRole_LastAdminDemoteRefused(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	ctx := context.Background()
	ca, _ := cRepo.Create(ctx, "alpha", "Alpha")
	uid := insertUser(t, aRepo, "solo@x.com")
	mid := joinCommunity(t, aRepo, uid, ca.ID, auth.RoleAdmin)

	postRole(h, mid, uid, string(auth.RoleMember))

	got, err := aRepo.MembershipByID(ctx, mid)
	if err != nil {
		t.Fatalf("membership by id: %v", err)
	}
	if got.Role != auth.RoleAdmin {
		t.Fatalf("last admin must stay admin, got %s", got.Role)
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

// TestPostCreateCommunity_SeedsDefaultChannel is the regression for the
// "load channel: sql: no rows in result set" crash: a runtime-created
// community must get its #general channel so the first chat visit resolves a
// channel instead of erroring on an empty result set.
func TestPostCreateCommunity_SeedsDefaultChannel(t *testing.T) {
	h, aRepo, cRepo := newTestHandler(t)
	chatRepo := chat.NewRepo(aRepo.DB)
	h.ChatRepo = chatRepo
	ctx := context.Background()

	uid := uuid.NewString()
	now := time.Now().Unix()
	if _, err := aRepo.DB.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		uid, "founder@x.com", "h", "active", now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	body := `{"sa_name":"Alpha","sa_slug":"alpha","sa_email":"founder@x.com"}`
	req := httptest.NewRequest(http.MethodPost, "/superadmin/community/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.PostCreateCommunity(httptest.NewRecorder(), req)

	c, err := cRepo.BySlug(ctx, "alpha")
	if err != nil {
		t.Fatalf("community must be created, got err: %v", err)
	}
	ch, err := chatRepo.DefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("new community must have a default channel, got err: %v", err)
	}
	if ch.Slug != "general" {
		t.Fatalf("default channel slug = %q, want %q", ch.Slug, "general")
	}
}
