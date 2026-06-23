package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
)

// recordingDeleter captures community ids passed to Delete so a test can assert
// solo-owned communities are torn down during erasure.
type recordingDeleter struct{ ids []string }

func (d *recordingDeleter) Delete(_ context.Context, communityID string) error {
	d.ids = append(d.ids, communityID)
	return nil
}

// seedVerifiedUser registers + verifies a user in the bootstrap community and
// returns their id.
func seedVerifiedUser(t *testing.T, svc *auth.Service, communityID, email, pass string) string {
	t.Helper()
	ctx := context.Background()
	code, err := svc.IssueInvite(ctx, communityID, nil, nil)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	reg, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: pass, InviteCode: code})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify %s: %v", email, err)
	}
	return reg.UserID
}

func TestDeleteAccount_AnonymisesAndPurges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)
	db := repo.DB

	uid := seedVerifiedUser(t, svc, communityID, "alice@example.com", "supersecret123")

	// content authored by alice across the community
	now := time.Now().Unix()
	msgID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_messages
		(id, community_id, author_id, kind, body_md, body_html, created_at)
		VALUES (?,?,?,?,?,?,?)`, msgID, communityID, uid, "user", "hi", "<p>hi</p>", now); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	thID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO threads
		(id, community_id, author_id, subject, body_md, body_html, last_activity_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, thID, communityID, uid, "Subj", "b", "<p>b</p>", now, now, now); err != nil {
		t.Fatalf("insert thread: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO bookmarks
		(id, user_id, community_id, chat_message_id, created_at) VALUES (?,?,?,?,?)`,
		uuid.NewString(), uid, communityID, msgID, now); err != nil {
		t.Fatalf("insert bookmark: %v", err)
	}

	if err := svc.DeleteAccount(ctx, uid); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	// email is gone — the original address no longer resolves
	if _, err := repo.UserByEmail(ctx, "alice@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("email should be erased, got err: %v", err)
	}
	// the row survives but is a disabled tombstone with no usable password
	u, err := repo.UserByID(ctx, uid)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Status != auth.StatusDisabled {
		t.Fatalf("status = %q, want disabled", u.Status)
	}
	if auth.CheckPassword(u.PasswordHash, "supersecret123") {
		t.Fatal("erased account must not accept the original password")
	}
	if u.Email == "alice@example.com" {
		t.Fatal("email not scrubbed")
	}

	// membership + content + personal rows are gone
	if _, err := repo.MembershipFor(ctx, uid, communityID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("membership should be gone, got err: %v", err)
	}
	for _, tbl := range []string{"chat_messages", "threads", "bookmarks"} {
		var n int
		col := "author_id"
		if tbl == "bookmarks" {
			col = "user_id"
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl+` WHERE `+col+` = ?`, uid).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("%s still has %d rows for erased user", tbl, n)
		}
	}
	// the content deletes enqueue RAG outbox 'delete' ops
	var del int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embed_outbox WHERE op = 'delete' AND ref_id IN (?, ?)`, msgID, thID).Scan(&del); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if del == 0 {
		t.Fatal("expected RAG outbox delete markers for erased content")
	}
}

func TestDeleteAccount_BlocksSoleOwnerWithMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, _ := setupSvc(t)
	cRepo := community.NewRepo(repo.DB)

	owner := seedVerifiedUser(t, svc, mustBoot(t, cRepo), "owner@example.com", "supersecret123")
	// a second community owned solely by `owner` but with another member
	shared, err := cRepo.Create(ctx, "shared", "Shared")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	other := seedVerifiedUser(t, svc, mustBoot(t, cRepo), "bob@example.com", "supersecret123")
	approved := time.Now()
	mustMember(t, repo, shared.ID, owner, auth.RoleOwner, approved)
	mustMember(t, repo, shared.ID, other, auth.RoleMember, approved)

	err = svc.DeleteAccount(ctx, owner)
	var soleErr *auth.SoleOwnerError
	if !errors.As(err, &soleErr) {
		t.Fatalf("want *SoleOwnerError, got %v", err)
	}
	if len(soleErr.Blockers) != 1 || soleErr.Blockers[0].Slug != "shared" {
		t.Fatalf("blockers = %+v, want [shared]", soleErr.Blockers)
	}
	// nothing was deleted — owner still resolves
	if _, err := repo.UserByEmail(ctx, "owner@example.com"); err != nil {
		t.Fatalf("owner must be untouched after a blocked erase: %v", err)
	}
}

func TestDeleteAccount_DeletesSoloOwnedCommunities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, _ := setupSvc(t)
	cRepo := community.NewRepo(repo.DB)
	rec := &recordingDeleter{}
	svc.Communities = rec

	uid := seedVerifiedUser(t, svc, mustBoot(t, cRepo), "solo@example.com", "supersecret123")
	solo, err := cRepo.Create(ctx, "solo", "Solo")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	mustMember(t, repo, solo.ID, uid, auth.RoleOwner, time.Now())

	if err := svc.DeleteAccount(ctx, uid); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(rec.ids) != 1 || rec.ids[0] != solo.ID {
		t.Fatalf("solo community delete = %v, want [%s]", rec.ids, solo.ID)
	}
}

func mustBoot(t *testing.T, cRepo *community.Repo) string {
	t.Helper()
	c, err := cRepo.BootstrapOrFetch(context.Background(), "test", "Test Community")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return c.ID
}

func mustMember(t *testing.T, repo *auth.Repo, communityID, userID string, role auth.Role, approved time.Time) {
	t.Helper()
	if err := repo.CreateMembership(context.Background(), nil, auth.Membership{
		ID: uuid.NewString(), UserID: userID, CommunityID: communityID,
		DisplayName: "m", Role: role, ApprovedAt: &approved,
	}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
}
