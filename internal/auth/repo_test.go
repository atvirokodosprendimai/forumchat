package auth_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupRepo(t *testing.T) (*auth.Repo, string) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test Community")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return auth.NewRepo(db), c.ID
}

func seedMember(t *testing.T, repo *auth.Repo, communityID, displayName string) string {
	t.Helper()
	ctx := context.Background()
	u := auth.User{
		ID:           uuid.NewString(),
		Email:        strings.ToLower(displayName) + "@x.test",
		PasswordHash: "x",
		Status:       auth.StatusActive,
	}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user %s: %v", displayName, err)
	}
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      u.ID,
		CommunityID: communityID,
		DisplayName: displayName,
		Role:        auth.RoleMember,
	}
	if err := repo.CreateMembership(ctx, nil, m); err != nil {
		t.Fatalf("create membership %s: %v", displayName, err)
	}
	return u.ID
}

func TestSearchMembersByDisplayName_Prefix(t *testing.T) {
	t.Parallel()
	repo, cid := setupRepo(t)
	aliceID := seedMember(t, repo, cid, "Alice")
	_ = seedMember(t, repo, cid, "Bob")
	albertID := seedMember(t, repo, cid, "Albert")

	hits, err := repo.SearchMembersByDisplayName(context.Background(), cid, "al", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 prefix hits, got %d (%+v)", len(hits), hits)
	}
	got := map[string]string{}
	for _, h := range hits {
		got[h.DisplayName] = h.UserID
	}
	if got["Alice"] != aliceID {
		t.Fatalf("missing/incorrect Alice: %v", got)
	}
	if got["Albert"] != albertID {
		t.Fatalf("missing/incorrect Albert: %v", got)
	}
}

func TestSearchMembersByDisplayName_Limit(t *testing.T) {
	t.Parallel()
	repo, cid := setupRepo(t)
	for _, n := range []string{"Sam1", "Sam2", "Sam3", "Sam4"} {
		seedMember(t, repo, cid, n)
	}
	hits, err := repo.SearchMembersByDisplayName(context.Background(), cid, "sa", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 (limit), got %d", len(hits))
	}
}

func TestSearchMembersByDisplayName_EmptyQuery(t *testing.T) {
	t.Parallel()
	repo, cid := setupRepo(t)
	seedMember(t, repo, cid, "Alice")
	hits, err := repo.SearchMembersByDisplayName(context.Background(), cid, "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("want 0 for empty query, got %d", len(hits))
	}
}

func TestCountAdmins(t *testing.T) {
	t.Parallel()
	repo, cid := setupRepo(t)
	ctx := context.Background()
	if n, err := repo.CountAdmins(ctx, cid); err != nil || n != 0 {
		t.Fatalf("baseline want 0 admins, got %d (err=%v)", n, err)
	}
	// seed two members, promote one to admin
	uA := seedMember(t, repo, cid, "Alice")
	_ = seedMember(t, repo, cid, "Bob")
	mA, err := repo.MembershipFor(ctx, uA, cid)
	if err != nil {
		t.Fatalf("membershipFor: %v", err)
	}
	if err := repo.UpdateMembershipRole(ctx, mA.ID, auth.RoleAdmin); err != nil {
		t.Fatalf("promote: %v", err)
	}
	n, err := repo.CountAdmins(ctx, cid)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 admin after promotion, got %d", n)
	}
}

func TestSearchMembersByDisplayName_OtherCommunityExcluded(t *testing.T) {
	t.Parallel()
	repo, cidA := setupRepo(t)
	ctx := context.Background()
	cB, err := community.NewRepo(repo.DB).BootstrapOrFetch(ctx, "other", "Other")
	if err != nil {
		t.Fatalf("2nd community: %v", err)
	}
	seedMember(t, repo, cidA, "Alice")
	seedMember(t, repo, cB.ID, "Alistair")
	hits, err := repo.SearchMembersByDisplayName(ctx, cidA, "al", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].DisplayName != "Alice" {
		t.Fatalf("want only Alice in cidA, got %+v", hits)
	}
}
