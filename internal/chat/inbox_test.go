package chat_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// TestRecentForCommunities_ScopesAndEmptySafety is the privacy guarantee behind
// the SaaS member chat inbox: a viewer sees ONLY their own communities' chats,
// an empty scope returns nothing (never the god-mode all-communities feed), and
// soft-deleted rows are hidden unless includeDeleted is set.
func TestRecentForCommunities_ScopesAndEmptySafety(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := community.NewRepo(db)
	repo := chat.NewRepo(db)

	a, err := cRepo.BootstrapOrFetch(ctx, "a", "Community A")
	if err != nil {
		t.Fatalf("community a: %v", err)
	}
	b, err := cRepo.BootstrapOrFetch(ctx, "b", "Community B")
	if err != nil {
		t.Fatalf("community b: %v", err)
	}
	if _, err := repo.EnsureDefaultChannel(ctx, a.ID); err != nil {
		t.Fatalf("channel a: %v", err)
	}
	if _, err := repo.EnsureDefaultChannel(ctx, b.ID); err != nil {
		t.Fatalf("channel b: %v", err)
	}

	insertMsg := func(cid, body string, deleted bool) string {
		id := uuid.NewString()
		if err := repo.Insert(ctx, chat.Message{
			ID: id, CommunityID: cid, Kind: chat.KindSystem,
			BodyMarkdown: body, BodyHTML: body, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert msg: %v", err)
		}
		if deleted {
			if _, err := db.ExecContext(ctx, `UPDATE chat_messages SET deleted_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
				t.Fatalf("soft-delete: %v", err)
			}
		}
		return id
	}
	msgA := insertMsg(a.ID, "hello A", false)
	msgADel := insertMsg(a.ID, "removed A", true)
	msgB := insertMsg(b.ID, "hello B", false)

	has := func(rows []chat.GlobalMessage, id string) bool {
		for _, m := range rows {
			if m.ID == id {
				return true
			}
		}
		return false
	}

	// Scoped to A only, deleted hidden: sees A's live message, not A's deleted
	// one, and never B's.
	rows, err := repo.RecentForCommunities(ctx, []string{a.ID}, 100, false)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if !has(rows, msgA) {
		t.Fatalf("scoped feed must include A's live message")
	}
	if has(rows, msgADel) {
		t.Fatalf("scoped feed must hide soft-deleted messages")
	}
	if has(rows, msgB) {
		t.Fatalf("scoped feed must NOT leak another community's messages")
	}

	// Empty scope → empty feed, NEVER the god-mode all-communities feed.
	rows, err = repo.RecentForCommunities(ctx, nil, 100, false)
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty scope must return no rows, got %d", len(rows))
	}

	// includeDeleted=true keeps the removed row; both communities visible.
	rows, err = repo.RecentForCommunities(ctx, []string{a.ID, b.ID}, 100, true)
	if err != nil {
		t.Fatalf("multi scope: %v", err)
	}
	if !has(rows, msgADel) || !has(rows, msgB) {
		t.Fatalf("includeDeleted feed must include deleted + both communities")
	}

	// God-mode reads every community but HIDES soft-deleted rows: a removed
	// message is invisible to everyone, super-admins included.
	all, err := repo.RecentGlobal(ctx, 100)
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if !has(all, msgA) || !has(all, msgB) {
		t.Fatalf("god-mode feed must include every community's live messages")
	}
	if has(all, msgADel) {
		t.Fatalf("god-mode feed must hide soft-deleted messages from super-admins too")
	}
}

// TestCommunityIDsForUser_ApprovedNonBannedOnly verifies the inbox scope source:
// only approved, non-banned memberships count, so a pending or banned member
// never streams that community's chat.
func TestCommunityIDsForUser_ApprovedNonBannedOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := community.NewRepo(db)
	aRepo := auth.NewRepo(db)

	approved, _ := cRepo.BootstrapOrFetch(ctx, "ok", "Approved")
	pending, _ := cRepo.BootstrapOrFetch(ctx, "pend", "Pending")
	banned, _ := cRepo.BootstrapOrFetch(ctx, "ban", "Banned")

	u := auth.User{ID: uuid.NewString(), Email: "u@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now()
	future := now.Add(24 * time.Hour)
	mk := func(cid string, approvedAt, bannedUntil *time.Time) {
		if err := aRepo.CreateMembership(ctx, nil, auth.Membership{
			ID: uuid.NewString(), UserID: u.ID, CommunityID: cid,
			DisplayName: "U", Role: auth.RoleMember,
			ApprovedAt: approvedAt, BannedUntil: bannedUntil,
		}); err != nil {
			t.Fatalf("membership: %v", err)
		}
	}
	mk(approved.ID, &now, nil)   // approved, not banned → included
	mk(pending.ID, nil, nil)     // not approved → excluded
	mk(banned.ID, &now, &future) // approved but currently banned → excluded

	ids, err := aRepo.CommunityIDsForUser(ctx, u.ID, now.Unix())
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if len(ids) != 1 || ids[0] != approved.ID {
		t.Fatalf("want only the approved, non-banned community, got %v", ids)
	}
}
