package auth_test

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// TestRoleRank pins the ordering member < moderator < admin < owner so the
// AtLeast gates compose: an owner satisfies every admin/mod/member bar, and an
// admin never satisfies the owner-only infra gate.
func TestRoleRank(t *testing.T) {
	t.Parallel()
	if !auth.RoleOwner.AtLeast(auth.RoleAdmin) {
		t.Fatal("owner must rank >= admin")
	}
	if !auth.RoleOwner.AtLeast(auth.RoleOwner) {
		t.Fatal("owner must satisfy the owner gate")
	}
	if auth.RoleAdmin.AtLeast(auth.RoleOwner) {
		t.Fatal("admin must NOT satisfy the owner gate")
	}
	if !auth.RoleOwner.AtLeast(auth.RoleMod) || !auth.RoleOwner.AtLeast(auth.RoleMember) {
		t.Fatal("owner must satisfy mod and member bars")
	}
	if auth.RoleMod.AtLeast(auth.RoleAdmin) {
		t.Fatal("moderator must NOT satisfy the admin gate")
	}
}

// TestUpdateMembershipRole_RoundTrip covers the persistence path behind
// the roster context-menu's "Make moderator" / "Remove moderator"
// actions: promote a member to moderator and demote back.
func TestUpdateMembershipRole_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)

	code, err := svc.IssueInvite(ctx, communityID, nil, nil)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	reg, err := svc.Register(ctx, auth.RegisterInput{
		Email: "carol@example.com", Password: "supersecret123", InviteCode: code,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Verify(ctx, reg.VerificationToken, communityID); err != nil {
		t.Fatalf("verify: %v", err)
	}

	m, err := repo.MembershipFor(ctx, reg.UserID, communityID)
	if err != nil {
		t.Fatalf("membership for: %v", err)
	}
	if m.Role != auth.RoleMember {
		t.Fatalf("new member want role=member, got %s", m.Role)
	}

	if err := repo.UpdateMembershipRole(ctx, m.ID, auth.RoleMod); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if got, _ := repo.MembershipByID(ctx, m.ID); got.Role != auth.RoleMod {
		t.Fatalf("after promote want role=moderator, got %s", got.Role)
	}

	if err := repo.UpdateMembershipRole(ctx, m.ID, auth.RoleMember); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if got, _ := repo.MembershipByID(ctx, m.ID); got.Role != auth.RoleMember {
		t.Fatalf("after demote want role=member, got %s", got.Role)
	}
}
