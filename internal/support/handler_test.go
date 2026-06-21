package support

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// newTestHandler opens a migrated temp DB, seeds the support community +
// a home community + two users, and returns a wired support.Handler plus
// the projects service (so tests can author reports the same way the
// PostReport handler does). Discards logs.
func newTestHandler(t *testing.T) (*Handler, *projects.Service, *community.Repo, string) {
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
	exec(`INSERT INTO communities (id, slug, name, created_at) VALUES ('sup','support','Support',0)`)
	exec(`INSERT INTO communities (id, slug, name, created_at) VALUES ('acme','acme','Acme Inc',0)`)
	exec(`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES ('uA','a@acme.test','x',0,0)`)
	exec(`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES ('uB','b@acme.test','x',0,0)`)

	cRepo := community.NewRepo(db)
	pRepo := projects.NewRepo(db)
	pSvc := projects.NewService(pRepo, projects.NewBus(), nil, 0)
	h := New("sup", cRepo, pSvc, pRepo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h, pSvc, cRepo, "sup"
}

// fileReport mimics PostReport's core: ensure the Inbox project, then
// create an issue authored by userID.
func fileReport(t *testing.T, h *Handler, svc *projects.Service, userID, title string) projects.Issue {
	t.Helper()
	ctx := context.Background()
	pid, err := svc.EnsureNamedProject(ctx, h.communityID, userID, inboxTitle, inboxDesc)
	if err != nil {
		t.Fatalf("ensure inbox: %v", err)
	}
	iss, err := svc.CreateIssue(ctx, pid, title, "body", projects.Identity{UserID: userID, Name: userID})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	return iss
}

// Before any report is filed, the Inbox project does not exist and the
// read-back is empty (lazy creation — a GET must never create it).
func TestFindInboxLazyAndEmptyReadback(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	ctx := context.Background()
	pid, err := h.findInboxProjectID(ctx)
	if err != nil {
		t.Fatalf("findInbox: %v", err)
	}
	if pid != "" {
		t.Fatalf("inbox project should not exist before first report, got %q", pid)
	}
	if got := h.myReports(ctx, "uA"); len(got) != 0 {
		t.Fatalf("expected no reports, got %d", len(got))
	}
}

// The core isolation property: a reporter reads back ONLY their own
// reports, and the ownedIssue guard rejects another user's report.
func TestReadBackIsOwnerScoped(t *testing.T) {
	h, svc, _, _ := newTestHandler(t)
	ctx := context.Background()

	aIssue := fileReport(t, h, svc, "uA", "A's first report")
	fileReport(t, h, svc, "uA", "A's second report")
	bIssue := fileReport(t, h, svc, "uB", "B's only report")

	if got := h.myReports(ctx, "uA"); len(got) != 2 {
		t.Fatalf("A should see 2 own reports, got %d", len(got))
	}
	if got := h.myReports(ctx, "uB"); len(got) != 1 {
		t.Fatalf("B should see 1 own report, got %d", len(got))
	}

	// A owns A's issue; B does NOT (the write-only guard).
	if _, ok := h.ownedIssue(ctx, aIssue.ID, "uA"); !ok {
		t.Fatalf("A must own their own report")
	}
	if _, ok := h.ownedIssue(ctx, aIssue.ID, "uB"); ok {
		t.Fatalf("B must NOT be able to open A's report")
	}
	// Symmetric.
	if _, ok := h.ownedIssue(ctx, bIssue.ID, "uA"); ok {
		t.Fatalf("A must NOT be able to open B's report")
	}
}

// ownedIssue must reject an issue that lives outside the support Inbox
// project, even for its own creator — so a member's normal project issue
// can never be reached through /report-issue.
func TestOwnedIssueRejectsNonInboxProject(t *testing.T) {
	h, svc, _, _ := newTestHandler(t)
	ctx := context.Background()

	// A normal project + issue in the SAME (support) community but not the
	// Inbox project.
	other, err := svc.CreateProject(ctx, "sup", "uA", "Some other project", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	iss, err := svc.CreateIssue(ctx, other.ID, "outside issue", "x", projects.Identity{UserID: "uA", Name: "uA"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	// Force the Inbox project to exist so findInboxProjectID resolves.
	fileReport(t, h, svc, "uA", "real report")

	if _, ok := h.ownedIssue(ctx, iss.ID, "uA"); ok {
		t.Fatalf("ownedIssue must reject an issue outside the Inbox project")
	}
}

// composeBody stamps the reporter's identity + home community so a
// super-admin reading the inbox knows which tenant filed the report.
func TestComposeBodyStampsTriageContext(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	id := projects.Identity{UserID: "uA", Name: "Alice"}
	aid := auth.Identity{
		User:       auth.User{ID: "uA", Email: "a@acme.test"},
		Membership: auth.Membership{CommunityID: "acme"},
	}
	body := h.composeBody(context.Background(), id, aid, "the screen is blank")
	for _, want := range []string{"Alice", "a@acme.test", "Acme Inc", "acme", "the screen is blank"} {
		if !strings.Contains(body, want) {
			t.Errorf("composeBody missing %q in:\n%s", want, body)
		}
	}
}
