package auth_test

import (
	"context"
	"testing"
)

// TestAdminDisplayNameOverride verifies the admin override hides the member's
// own name from everyone else (ListMembers effective name, @mention search and
// resolution) while keeping the member's own name as the fallback once cleared.
func TestAdminDisplayNameOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, cid := setupRepo(t)

	userID := seedMember(t, repo, cid, "uglyname123")
	m, err := repo.MembershipFor(ctx, userID, cid)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := repo.ApproveMembership(ctx, m.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}

	if err := repo.SetAdminDisplayName(ctx, m.ID, "  Clean Name  "); err != nil {
		t.Fatalf("set nick: %v", err)
	}

	// ListMembers carries own name, override and resolved effective name.
	members, err := repo.ListMembers(ctx, cid)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	var found bool
	for _, mr := range members {
		if mr.UserID != userID {
			continue
		}
		found = true
		if mr.DisplayName != "uglyname123" {
			t.Fatalf("own name should be preserved, got %q", mr.DisplayName)
		}
		if mr.AdminDisplayName != "Clean Name" {
			t.Fatalf("override not trimmed/stored, got %q", mr.AdminDisplayName)
		}
		if mr.EffectiveDisplayName != "Clean Name" {
			t.Fatalf("effective should be the override, got %q", mr.EffectiveDisplayName)
		}
	}
	if !found {
		t.Fatal("member not in ListMembers")
	}

	// @mention typeahead matches the override, not the hidden own name.
	hits, err := repo.SearchMembersByDisplayName(ctx, cid, "clean", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].DisplayName != "Clean Name" || hits[0].UserID != userID {
		t.Fatalf("search by override failed: %+v", hits)
	}
	if hidden, _ := repo.SearchMembersByDisplayName(ctx, cid, "ugly", 10); len(hidden) != 0 {
		t.Fatalf("own name should not surface in typeahead, got %+v", hidden)
	}

	// @mention resolution maps the override token to the user.
	ids, err := repo.UserIDsByDisplayName(ctx, cid, []string{"clean name"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(ids) != 1 || ids[0] != userID {
		t.Fatalf("override should resolve to user, got %v", ids)
	}

	// Clearing the override falls back to the member's own name everywhere.
	if err := repo.SetAdminDisplayName(ctx, m.ID, ""); err != nil {
		t.Fatalf("clear nick: %v", err)
	}
	members, _ = repo.ListMembers(ctx, cid)
	for _, mr := range members {
		if mr.UserID == userID && mr.EffectiveDisplayName != "uglyname123" {
			t.Fatalf("effective should fall back to own name, got %q", mr.EffectiveDisplayName)
		}
	}
}
