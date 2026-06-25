package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// serviceSentinelHash is parked in password_hash for a NON-HUMAN service
// account (today: an external-chat-bot connector's synthetic member). Like the
// OAuth and erased-account sentinels it is not a valid bcrypt hash, so
// CheckPassword always fails and HasPassword reports false — the account exists
// to author messages and appear in the roster, never to log in.
const serviceSentinelHash = "service-account-no-login"

// CreateServiceAccount provisions a non-human member: an active, login-disabled
// user (synthetic unguessable email) plus an APPROVED membership in communityID
// with the given display name. It is the seam behind a connector's "acts as a
// human" identity (spec-connectors) — the connector posts as the returned user
// id, so its messages are ordinary kind='user' messages with full member
// machinery (roster, @mention, profile, mod-delete) and no special-casing.
//
// Always approved: the account is operator-created, not a public signup, so it
// must never land in the pending queue regardless of the community's join policy.
func (s *Service) CreateServiceAccount(ctx context.Context, communityID, displayName, avatar string) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return "", fmt.Errorf("service account: empty display name")
	}
	userID := uuid.NewString()
	// A unique, unroutable email satisfies the NOT NULL/UNIQUE constraint without
	// colliding with a real address or being deliverable (no magic-link login).
	email := "connector-" + userID + "@connector.invalid"
	if err := s.Repo.CreateUser(ctx, User{
		ID:           userID,
		Email:        email,
		PasswordHash: serviceSentinelHash,
		Status:       StatusActive,
	}); err != nil {
		return "", fmt.Errorf("service account user: %w", err)
	}
	now := time.Now()
	if err := s.Repo.CreateMembership(ctx, nil, Membership{
		ID:          uuid.NewString(),
		UserID:      userID,
		CommunityID: communityID,
		DisplayName: displayName,
		AvatarURL:   avatar,
		Role:        RoleMember,
		ApprovedAt:  &now,
	}); err != nil {
		return "", fmt.Errorf("service account membership: %w", err)
	}
	return userID, nil
}

// RenameServiceMember updates a service account's display name + avatar across
// its membership(s). Used when an admin renames a connector.
func (s *Service) RenameServiceMember(ctx context.Context, userID, displayName, avatar string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return fmt.Errorf("service account: empty display name")
	}
	return s.Repo.UpdateAllMembershipProfiles(ctx, userID, displayName, avatar)
}

// RemoveServiceAccount deletes a service account's membership(s) and user row.
// Its authored chat messages survive as a "deleted member" (chat_messages.
// author_id is ON DELETE SET NULL), so channel history stays readable — the same
// trade-off as account erasure (§5g), but here we keep the content rather than
// scrubbing it. Used when an admin deletes a connector.
func (s *Service) RemoveServiceAccount(ctx context.Context, userID string) error {
	return s.Repo.DeleteServiceAccount(ctx, userID)
}

// DeleteServiceAccount removes a synthetic account's memberships and user row in
// one transaction. Safe because a service account only ever authors chat
// messages (author_id SET NULL on delete); it never owns projects/lobbies/etc.
// that carry a RESTRICT FK. Not for human accounts — use EraseUser for those.
func (r *Repo) DeleteServiceAccount(ctx context.Context, userID string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memberships WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete service memberships: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID); err != nil {
		return fmt.Errorf("delete service user: %w", err)
	}
	return tx.Commit()
}
