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
	saved, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Title: "Hi", Body: "# Heading\n\nbody", Visibility: "public"})
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
	first, _ := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Body: "one", Visibility: "private"})
	second, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Body: "two", Visibility: "private"})
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
	if _, err := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Body: "   "}); err != ErrEmpty {
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

	if _, err := svc.Save(ctx, other, SaveInput{ID: n.ID, Body: "x"}); err != ErrForbidden {
		t.Fatalf("Save by a non-editor must be ErrForbidden, got %v", err)
	}
}

func TestByShareTokenRoundTrip(t *testing.T) {
	svc, repo, cid, uid := setup(t)
	ctx := context.Background()
	n, _ := svc.CreateDraft(ctx, cid, "", uid)
	saved, _ := svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Body: "secret", Visibility: "private"})

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
	svc.Save(ctx, authorID(uid), SaveInput{ID: n.ID, Body: "para one\n\npara two", Visibility: "public"})

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
