package pastes_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/pastes"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// pasteTestEnv opens a migrated temp DB, bootstraps a community + #general
// channel + a user, and returns the service, community id, channel id, user id.
func pasteTestEnv(t *testing.T) (*pastes.Service, string, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
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
	u := auth.User{ID: uuid.NewString(), Email: "author@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch, err := chat.NewRepo(db).EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	return pastes.NewService(pastes.NewRepo(db)), c.ID, ch.ID, u.ID
}

func TestSave_Markdown(t *testing.T) {
	t.Parallel()
	svc, cid, chID, uid := pasteTestEnv(t)
	ctx := context.Background()

	draft, err := svc.CreateDraft(ctx, cid, chID, uid)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if !draft.IsDraft() {
		t.Fatal("fresh paste should be a draft")
	}

	saved, err := svc.Save(ctx, pastes.SaveInput{
		ID: draft.ID, CommunityID: cid, AuthorID: uid,
		Title: "Notes", Language: "markdown", Body: "# Hello\n\nworld",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.IsDraft() {
		t.Fatal("saved paste should no longer be a draft")
	}
	if !strings.Contains(saved.BodyHTML, "<h1") {
		t.Fatalf("markdown not rendered to a heading: %q", saved.BodyHTML)
	}
}

func TestSave_CodeFenced(t *testing.T) {
	t.Parallel()
	svc, cid, chID, uid := pasteTestEnv(t)
	ctx := context.Background()

	draft, _ := svc.CreateDraft(ctx, cid, chID, uid)
	// Body contains a triple-backtick run; the fence must be longer so the
	// code can't break out of its block.
	body := "func main() {}\n```\nstill code\n```"
	saved, err := svc.Save(ctx, pastes.SaveInput{
		ID: draft.ID, CommunityID: cid, AuthorID: uid, Language: "go", Body: body,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(saved.BodyHTML, "<pre>") && !strings.Contains(saved.BodyHTML, "<pre ") {
		t.Fatalf("code not rendered as a <pre> block: %q", saved.BodyHTML)
	}
	if !strings.Contains(saved.BodyHTML, "still code") {
		t.Fatalf("embedded backticks broke out of the fence: %q", saved.BodyHTML)
	}
}

func TestSave_Empty(t *testing.T) {
	t.Parallel()
	svc, cid, chID, uid := pasteTestEnv(t)
	ctx := context.Background()
	draft, _ := svc.CreateDraft(ctx, cid, chID, uid)
	if _, err := svc.Save(ctx, pastes.SaveInput{ID: draft.ID, CommunityID: cid, AuthorID: uid, Body: "   "}); !errors.Is(err, pastes.ErrEmpty) {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestSave_AlreadyPosted(t *testing.T) {
	t.Parallel()
	svc, cid, chID, uid := pasteTestEnv(t)
	ctx := context.Background()
	draft, _ := svc.CreateDraft(ctx, cid, chID, uid)
	if _, err := svc.Save(ctx, pastes.SaveInput{ID: draft.ID, CommunityID: cid, AuthorID: uid, Body: "x"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := svc.Save(ctx, pastes.SaveInput{ID: draft.ID, CommunityID: cid, AuthorID: uid, Body: "y"}); !errors.Is(err, pastes.ErrNotDraft) {
		t.Fatalf("want ErrNotDraft, got %v", err)
	}
}

func TestSave_Forbidden(t *testing.T) {
	t.Parallel()
	svc, cid, chID, uid := pasteTestEnv(t)
	ctx := context.Background()
	draft, _ := svc.CreateDraft(ctx, cid, chID, uid)
	if _, err := svc.Save(ctx, pastes.SaveInput{ID: draft.ID, CommunityID: cid, AuthorID: "someone-else", Body: "x"}); !errors.Is(err, pastes.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}
