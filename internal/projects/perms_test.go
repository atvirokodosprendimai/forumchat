package projects

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// newPermsTestSvc opens a migrated temp DB, seeds one community + three
// users (creator, member, other), and returns the wired service + repo.
func newPermsTestSvc(t *testing.T) (*Service, *Repo) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO communities (id, slug, name, created_at) VALUES ('c','c','C',0)`)
	for _, u := range []string{"creator", "member", "other"} {
		exec(`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES (?,?,?,0,0)`, u, u+"@t.test", "x")
	}
	repo := NewRepo(db)
	return NewService(repo, NewBus(), nil, 0), repo
}

// TestRestrictedVisibilityAndGrants verifies the index filter hides a
// restricted project from non-granted members but shows it to the creator,
// admins, and granted members — and that a grant resolves to the right
// access level.
func TestRestrictedVisibilityAndGrants(t *testing.T) {
	svc, repo := newPermsTestSvc(t)
	ctx := context.Background()

	open, err := svc.CreateProject(ctx, "c", "creator", "Open", "", PermOpts{})
	if err != nil {
		t.Fatalf("create open: %v", err)
	}
	hidden, err := svc.CreateProject(ctx, "c", "creator", "Hidden", "", PermOpts{
		NeedsPerms: true, Visibility: VisibilityRestricted, MemberAccess: AccessRead,
	})
	if err != nil {
		t.Fatalf("create hidden: %v", err)
	}

	// "other" is a plain member with no grant: sees only the open project.
	visible, err := repo.ListVisibleForCommunity(ctx, "c", "other", false, false)
	if err != nil {
		t.Fatalf("list visible: %v", err)
	}
	if got := titles(visible); !(len(got) == 1 && got[0] == "Open") {
		t.Fatalf("non-granted member should see only [Open], got %v", got)
	}

	// Creator sees both.
	if got := titles(mustList(t, repo, "creator", false)); len(got) != 2 {
		t.Fatalf("creator should see both projects, got %v", got)
	}
	// Admin (isAdmin=true) sees both regardless of grants.
	if got := titles(mustList(t, repo, "other", true)); len(got) != 2 {
		t.Fatalf("admin should see both projects, got %v", got)
	}

	// Grant "other" read on the hidden project → now visible to them.
	if err := svc.GrantMember(ctx, hidden.ID, "creator", false, "other", AccessRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := titles(mustList(t, repo, "other", false)); len(got) != 2 {
		t.Fatalf("granted member should see both, got %v", got)
	}

	// The grant resolves to read; effective access denies writes.
	grant, ok := repo.MemberAccessFor(ctx, hidden.ID, "other")
	if !ok || grant != AccessRead {
		t.Fatalf("MemberAccessFor = %q,%v; want read,true", grant, ok)
	}
	p, _ := repo.ByID(ctx, hidden.ID)
	caller := Identity{UserID: "other", Role: auth.RoleMember}
	if acc := EffectiveAccess(p, caller, grant, ok); acc != AccessReadOnly {
		t.Fatalf("read grant on restricted = %d, want read-only", acc)
	}

	// Upgrade to write, then revoke.
	if err := svc.GrantMember(ctx, hidden.ID, "creator", false, "other", AccessWrite); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	if g, _ := repo.MemberAccessFor(ctx, hidden.ID, "other"); g != AccessWrite {
		t.Fatalf("grant after upgrade = %q, want write", g)
	}
	if err := svc.RevokeMember(ctx, hidden.ID, "creator", false, "other"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := repo.MemberAccessFor(ctx, hidden.ID, "other"); ok {
		t.Fatalf("grant should be gone after revoke")
	}

	// The open project is unaffected by all of this.
	if p, _ := repo.ByID(ctx, open.ID); p.NeedsPerms {
		t.Fatalf("open project must stay needs_perms=false")
	}
}

// TestManageGate confirms a non-creator, non-admin member cannot change
// perms or grants.
func TestManageGate(t *testing.T) {
	svc, _ := newPermsTestSvc(t)
	ctx := context.Background()
	p, _ := svc.CreateProject(ctx, "c", "creator", "P", "", PermOpts{})

	if err := svc.SetPerms(ctx, p.ID, "member", false, true, VisibilityRestricted, AccessRead); err != ErrForbidden {
		t.Fatalf("SetPerms by non-manager = %v, want ErrForbidden", err)
	}
	if err := svc.GrantMember(ctx, p.ID, "member", false, "other", AccessWrite); err != ErrForbidden {
		t.Fatalf("GrantMember by non-manager = %v, want ErrForbidden", err)
	}
	// Admin may manage.
	if err := svc.SetPerms(ctx, p.ID, "member", true, true, VisibilityCommunity, AccessWrite); err != nil {
		t.Fatalf("SetPerms by admin: %v", err)
	}
}

func mustList(t *testing.T, repo *Repo, userID string, isAdmin bool) []IndexRow {
	t.Helper()
	rows, err := repo.ListVisibleForCommunity(context.Background(), "c", userID, isAdmin, false)
	if err != nil {
		t.Fatalf("list visible: %v", err)
	}
	return rows
}

func titles(rows []IndexRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}
