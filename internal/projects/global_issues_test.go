package projects

import (
	"context"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// setupGlobalRepo opens a migrated temp DB and seeds two communities, a
// user, and a mix of active / archived projects plus issues so the
// global-issues picker queries have something to chew on.
func setupGlobalRepo(t *testing.T) *Repo {
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
	// "Bravo" sorts after "Alpha" by name, but we insert it first to prove
	// ProjectsForCommunities orders by community name, not insertion order.
	exec(`INSERT INTO communities (id, slug, name, created_at) VALUES ('c2','bravo','Bravo',0)`)
	exec(`INSERT INTO communities (id, slug, name, created_at) VALUES ('c1','alpha','Alpha',0)`)
	exec(`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES ('u1','e@e','x',0,0)`)
	// Alpha: two active projects (p1 newer) + one archived (excluded).
	exec(`INSERT INTO projects (id, community_id, creator_user_id, title, created_at, updated_at) VALUES ('p1','c1','u1','Apollo',0,200)`)
	exec(`INSERT INTO projects (id, community_id, creator_user_id, title, created_at, updated_at) VALUES ('p2','c1','u1','Zephyr',0,100)`)
	exec(`INSERT INTO projects (id, community_id, creator_user_id, title, archived_at, created_at, updated_at) VALUES ('p3','c1','u1','Archived',1,0,300)`)
	// Bravo: no projects at all.
	// Issues on p1: two open (one triaged counts as open), one closed (excluded).
	exec(`INSERT INTO project_issues (id, project_id, title, status, creator_name, created_at, updated_at) VALUES ('i1','p1','bug','open','n',0,0)`)
	exec(`INSERT INTO project_issues (id, project_id, title, status, creator_name, created_at, updated_at) VALUES ('i2','p1','triage','triaged','n',0,0)`)
	exec(`INSERT INTO project_issues (id, project_id, title, status, creator_name, created_at, updated_at) VALUES ('i3','p1','done','closed','n',0,0)`)
	return NewRepo(db)
}

func TestProjectsForCommunities(t *testing.T) {
	repo := setupGlobalRepo(t)
	rows, err := repo.ProjectsForCommunities(context.Background(), []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	groups := groupCommunityProjects(rows)
	if len(groups) != 2 {
		t.Fatalf("want 2 community groups, got %d: %+v", len(groups), groups)
	}

	// Ordered by community name: Alpha before Bravo.
	if groups[0].CommunityName != "Alpha" || groups[1].CommunityName != "Bravo" {
		t.Fatalf("communities out of order: %q, %q", groups[0].CommunityName, groups[1].CommunityName)
	}

	alpha := groups[0]
	if len(alpha.Projects) != 2 {
		t.Fatalf("Alpha should have 2 active projects (archived excluded), got %d: %+v", len(alpha.Projects), alpha.Projects)
	}
	// Ordered by updated_at DESC: Apollo (200) before Zephyr (100).
	if alpha.Projects[0].ProjectTitle != "Apollo" || alpha.Projects[1].ProjectTitle != "Zephyr" {
		t.Fatalf("projects out of order: %+v", alpha.Projects)
	}
	// Open-issue count excludes the closed issue.
	if alpha.Projects[0].OpenIssues != 2 {
		t.Fatalf("Apollo open issues: want 2, got %d", alpha.Projects[0].OpenIssues)
	}
	if alpha.Projects[1].OpenIssues != 0 {
		t.Fatalf("Zephyr open issues: want 0, got %d", alpha.Projects[1].OpenIssues)
	}

	// A community with no active projects still appears, with no project rows.
	bravo := groups[1]
	if len(bravo.Projects) != 0 {
		t.Fatalf("Bravo should have no projects, got %d: %+v", len(bravo.Projects), bravo.Projects)
	}
	if bravo.CommunitySlug != "bravo" {
		t.Fatalf("Bravo slug: want bravo, got %q", bravo.CommunitySlug)
	}
}

// TestGlobalIssuesPageRender proves the picker markup reaches the page:
// the community name, project title, and — critically — the deep-link
// into that project's create-issue composer.
func TestGlobalIssuesPageRender(t *testing.T) {
	repo := setupGlobalRepo(t)
	rows, err := repo.ProjectsForCommunities(context.Background(), []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var sb strings.Builder
	err = webtempl.GlobalIssuesPage(webtempl.GlobalIssuesPageData{
		Viewer:      webtempl.Viewer{IsAuthed: true, IsAdminOfAnyCommunity: true},
		Communities: groupCommunityProjects(rows),
	}).Render(context.Background(), &sb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	for _, want := range []string{
		"Open an issue",
		"Alpha", "Bravo", "Apollo", "Zephyr",
		`/c/alpha/projects/p1/issues#new-issue`,
		"+ New issue",
		"No projects yet", // Bravo has none
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered /issues page missing %q", want)
		}
	}
}
