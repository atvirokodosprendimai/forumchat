package notes

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// setup opens a migrated tmp DB and bootstraps a community + a user, returning
// the service, repo, community id and user id.
func setup(t *testing.T) (*Service, *Repo, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	aRepo := auth.NewRepo(db)
	u := auth.User{ID: uuid.NewString(), Email: "author@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := aRepo.CreateMembership(ctx, tx, auth.Membership{
		ID: uuid.NewString(), UserID: u.ID, CommunityID: c.ID, DisplayName: "Alice", Role: auth.RoleMember,
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := NewRepo(db)
	return NewService(repo), repo, c.ID, u.ID
}

func authorID(uid string) auth.Identity {
	return auth.Identity{User: auth.User{ID: uid}, Membership: auth.Membership{Role: auth.RoleMember, DisplayName: "Alice"}}
}

// memberID builds a plain-member identity for a real user row.
func memberID(uid, name string) auth.Identity {
	return auth.Identity{User: auth.User{ID: uid}, Membership: auth.Membership{Role: auth.RoleMember, DisplayName: name}}
}

// addMember inserts a real user + membership (needed for the note_edit_requests
// FK on user_id) and returns the user id.
func addMember(t *testing.T, repo *Repo, cid, email, name string, role auth.Role) string {
	t.Helper()
	ctx := context.Background()
	aRepo := auth.NewRepo(repo.DB)
	u := auth.User{ID: uuid.NewString(), Email: email, PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tx, err := repo.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := aRepo.CreateMembership(ctx, tx, auth.Membership{
		ID: uuid.NewString(), UserID: u.ID, CommunityID: cid, DisplayName: name, Role: role,
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return u.ID
}

func TestSaveRendersAndMintsToken(t *testing.T) {
	svc, _, cid, uid := setup(t)
	ctx := context.Background()
	n, err := svc.CreateDraft(ctx, cid, "", uid)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if n.ShareToken == "" {
		t.Fatalf("draft should mint a share token up-front")
	}
	saved, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Title: "Hi", Body: "# Heading\n\nbody", Visibility: "public"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.ShareToken == "" {
		t.Fatalf("save must mint a share token")
	}
	if !saved.IsPublic() {
		t.Fatalf("want public, got %q", saved.Visibility)
	}
	if saved.BodyHTML == "" || saved.BodyHTML == saved.Body {
		t.Fatalf("body should render to html, got %q", saved.BodyHTML)
	}
}

func TestSaveTokenStableAcrossSaves(t *testing.T) {
	svc, _, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	first, _ := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "one", Visibility: "private"})
	second, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "two", Visibility: "private"})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if first.ShareToken != second.ShareToken {
		t.Fatalf("token must be stable: %q != %q", first.ShareToken, second.ShareToken)
	}
}

func TestSaveEmptyRejected(t *testing.T) {
	svc, _, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	if _, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "   "}); err != ErrEmpty {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestCanEditMatrix(t *testing.T) {
	svc, _, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)

	other := auth.Identity{User: auth.User{ID: "other"}, Membership: auth.Membership{Role: auth.RoleMember}}
	if n.CanEdit(other) {
		t.Fatalf("a plain member who isn't the author must not edit")
	}
	mod := auth.Identity{User: auth.User{ID: "mod"}, Membership: auth.Membership{Role: auth.RoleMod}}
	if !n.CanEdit(mod) {
		t.Fatalf("a moderator must be able to edit")
	}
	if !n.CanEdit(authorID(uid)) {
		t.Fatalf("the author must be able to edit")
	}
	super := auth.Identity{User: auth.User{ID: "sa"}, Membership: auth.Membership{Role: auth.RoleMember}, IsSuperAdmin: true}
	if !n.CanEdit(super) {
		t.Fatalf("super-admin must be able to edit")
	}

	if _, err := svc.Save(ctx, other, SaveInput{ID: n.ID, CommunityID: cid, Body: "x"}); err != ErrForbidden {
		t.Fatalf("Save by a non-editor must be ErrForbidden, got %v", err)
	}
}

// A moderator of a DIFFERENT community must not be able to save this note even
// though their role clears the CanEdit role gate — the community guard wins.
func TestSaveCrossCommunityRejected(t *testing.T) {
	svc, _, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)

	otherCommunityMod := auth.Identity{User: auth.User{ID: "mod-of-B"}, Membership: auth.Membership{Role: auth.RoleMod}}
	if _, err := svc.Save(ctx, otherCommunityMod, SaveInput{ID: n.ID, CommunityID: "community-B", Body: "pwn"}); err != ErrForbidden {
		t.Fatalf("cross-community save must be ErrForbidden, got %v", err)
	}
}

// TestRequestEditGrantFlow walks the whole request-to-edit lifecycle: a plain
// member requests, the author approves into a grant (CanEdit flips true and the
// member can Save), and the author revokes (CanEdit flips back). It also pins
// the security-critical invariants: a granted member cannot grant others, a
// duplicate request is idempotent, and re-requesting once an editor is rejected.
func TestRequestEditGrantFlow(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "shared doc", Visibility: "public"})

	bobID := addMember(t, repo, cid, "bob@x.test", "Bob", auth.RoleMember)
	bob := memberID(bobID, "Bob")

	cur, _ := repo.ByID(ctx, n.ID)
	if cur.CanEdit(bob) {
		t.Fatalf("a plain member must not edit before any grant")
	}

	// Bob requests; pending recorded but still no edit.
	if _, err := svc.RequestEdit(ctx, cid, bob, n.ID, "I'll fix typos"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if st, _ := repo.EditRequestStatus(ctx, n.ID, bobID); st != RequestPending {
		t.Fatalf("want pending, got %q", st)
	}
	cur, _ = repo.ByID(ctx, n.ID)
	if cur.CanEdit(bob) {
		t.Fatalf("a pending request must not confer edit rights")
	}

	// Duplicate request is idempotent — exactly one pending row.
	if _, err := svc.RequestEdit(ctx, cid, bob, n.ID, "again"); err != nil {
		t.Fatalf("duplicate request: %v", err)
	}
	if pend, _ := repo.ListEditRequests(ctx, n.ID, RequestPending); len(pend) != 1 {
		t.Fatalf("duplicate request created %d rows, want 1", len(pend))
	}

	// A non-manager (Bob) must not be able to grant — even himself.
	if _, err := svc.DecideEditRequest(ctx, cid, bob, n.ID, bobID, true); err != ErrForbidden {
		t.Fatalf("non-manager decide must be ErrForbidden, got %v", err)
	}

	// Author approves → Bob can edit and Save.
	after, err := svc.DecideEditRequest(ctx, cid, authorID(uid), n.ID, bobID, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !after.CanEdit(bob) {
		t.Fatalf("bob should edit after the grant")
	}
	if st, _ := repo.EditRequestStatus(ctx, n.ID, bobID); st != RequestGranted {
		t.Fatalf("want granted, got %q", st)
	}
	if _, err := svc.Save(ctx, bob, SaveInput{ID: n.ID, CommunityID: cid, Body: "edited by bob", Visibility: "public"}); err != nil {
		t.Fatalf("granted editor Save: %v", err)
	}

	// Re-requesting once already an editor is rejected.
	if _, err := svc.RequestEdit(ctx, cid, bob, n.ID, ""); err != ErrAlreadyEditor {
		t.Fatalf("re-request as editor must be ErrAlreadyEditor, got %v", err)
	}

	// Author revokes → grant gone, edit rights gone.
	rev, err := svc.DecideEditRequest(ctx, cid, authorID(uid), n.ID, bobID, false)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rev.CanEdit(bob) {
		t.Fatalf("bob must not edit after revoke")
	}
	if st, _ := repo.EditRequestStatus(ctx, n.ID, bobID); st != "" {
		t.Fatalf("revoke must delete the row, got %q", st)
	}
}

// TestRequestEditGuards covers the request-side authorization edges: a private
// note is not requestable by a non-editor (no existence oracle), and a forged
// community id is refused.
func TestRequestEditGuards(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	priv, _ := svc.CreateDraft(ctx, cid, "", uid)
	svc.Save(ctx, authorID(uid), SaveInput{ID: priv.ID, CommunityID: cid, Body: "secret", Visibility: "private"})
	pub, _ := svc.CreateDraft(ctx, cid, "", uid)
	svc.Save(ctx, authorID(uid), SaveInput{ID: pub.ID, CommunityID: cid, Body: "open", Visibility: "public"})

	bobID := addMember(t, repo, cid, "bob@x.test", "Bob", auth.RoleMember)
	bob := memberID(bobID, "Bob")

	if _, err := svc.RequestEdit(ctx, cid, bob, priv.ID, ""); err != ErrForbidden {
		t.Fatalf("request on a private note by a non-editor must be ErrForbidden, got %v", err)
	}
	if _, err := svc.RequestEdit(ctx, "community-B", bob, pub.ID, ""); err != ErrForbidden {
		t.Fatalf("cross-community request must be ErrForbidden, got %v", err)
	}
}

// TestGrantedCollaboratorCannotManage pins the privilege boundary: a granted
// collaborator may edit the body, but must NOT flip visibility (a distribution
// decision) or moderate another member's comment (a management action).
func TestGrantedCollaboratorCannotManage(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "doc", Visibility: "public"})

	bobID := addMember(t, repo, cid, "bob@x.test", "Bob", auth.RoleMember)
	bob := memberID(bobID, "Bob")
	if _, err := svc.RequestEdit(ctx, cid, bob, n.ID, ""); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := svc.DecideEditRequest(ctx, cid, authorID(uid), n.ID, bobID, true); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Bob can edit the body, but a forged note_visibility="private" is ignored —
	// only a manager changes visibility.
	saved, err := svc.Save(ctx, bob, SaveInput{ID: n.ID, CommunityID: cid, Body: "edited by bob", Visibility: "private"})
	if err != nil {
		t.Fatalf("granted collaborator Save: %v", err)
	}
	if !saved.IsPublic() {
		t.Fatalf("a granted collaborator must not change visibility; want public, got %q", saved.Visibility)
	}

	// The author comments; Bob (granted, not a manager) cannot moderate it, but
	// the author can.
	c, err := svc.AddComment(ctx, cid, authorID(uid), CommentInput{NoteID: n.ID, BlockIndex: 0, Body: "author note"})
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}
	fresh, _ := repo.ByID(ctx, n.ID)
	cmt, _ := repo.CommentByID(ctx, c.ID)
	if cmt.CanModerate(bob, fresh) {
		t.Fatalf("a granted collaborator must not moderate another member's comment")
	}
	if !cmt.CanModerate(authorID(uid), fresh) {
		t.Fatalf("the comment author/manager must be able to moderate it")
	}
}

func TestByShareTokenRoundTrip(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	saved, _ := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "secret", Visibility: "private"})

	got, err := repo.ByShareToken(ctx, saved.ShareToken)
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if got.ID != n.ID {
		t.Fatalf("token resolved wrong note")
	}
}

func TestAddAndListComments(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, CommunityID: cid, Body: "para one\n\npara two", Visibility: "public"})

	if _, err := svc.AddComment(ctx, cid, authorID(uid), CommentInput{NoteID: n.ID, BlockIndex: 1, Quote: "two", Body: "nice"}); err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if _, err := svc.AddComment(ctx, cid, authorID(uid), CommentInput{NoteID: n.ID, BlockIndex: 0, Body: "line comment"}); err != nil {
		t.Fatalf("add line comment: %v", err)
	}
	cs, err := svc.Repo.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 comments, got %d", len(cs))
	}
	// ordered by block_index: the line comment (block 0) comes first.
	if cs[0].BlockIndex != 0 || cs[1].BlockIndex != 1 {
		t.Fatalf("comments not ordered by block: %+v", cs)
	}
	if cs[0].AuthorName != "Alice" {
		t.Fatalf("author name not joined, got %q", cs[0].AuthorName)
	}
	_ = repo

	// empty comment rejected
	if _, err := svc.AddComment(ctx, cid, authorID(uid), CommentInput{NoteID: n.ID, Body: " "}); err != ErrEmpty {
		t.Fatalf("empty comment must be ErrEmpty, got %v", err)
	}
	// cross-community comment rejected
	if _, err := svc.AddComment(ctx, "other-community", authorID(uid), CommentInput{NoteID: n.ID, Body: "x"}); err != ErrForbidden {
		t.Fatalf("cross-community comment must be ErrForbidden, got %v", err)
	}
}
