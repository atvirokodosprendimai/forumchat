package auth

import "time"

// Account-age limits gate brand-new signups against spam. Both key off
// users.created_at, so an established account joining a new community is never
// throttled — only freshly registered ones are.
const (
	// NewUserPostDelay is how long after registration a member must wait before
	// posting their first chat message.
	NewUserPostDelay = 1 * time.Minute
	// NewUserCommunityDelay is how long after registration a user must wait
	// before creating (or requesting) a community via the self-serve flow.
	NewUserCommunityDelay = 5 * time.Minute
)

type UserStatus string

const (
	StatusPending  UserStatus = "pending"
	StatusActive   UserStatus = "active"
	StatusInvited  UserStatus = "invited" // admin-added, awaiting set-password via signup token
	StatusDisabled UserStatus = "disabled"
)

type Role string

const (
	RoleMember Role = "member"
	RoleMod    Role = "moderator"
	RoleAdmin  Role = "admin"
	// RoleOwner is the per-community super-admin: the top community role,
	// above admin. In SaaS mode the owner alone configures tenant infra
	// (ai_enabled, RAG model/host/collection, translate, storage, join
	// policy). In self-hosted mode it is inert — an admin is the de-facto top.
	RoleOwner Role = "owner"
)

func (r Role) AtLeast(min Role) bool {
	rank := map[Role]int{RoleMember: 0, RoleMod: 1, RoleAdmin: 2, RoleOwner: 3}
	return rank[r] >= rank[min]
}

type User struct {
	ID           string
	Email        string
	PasswordHash string
	Status       UserStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasPassword reports whether the user has a usable password (login works with
// it). It is false for OAuth-only accounts, whose password_hash is the
// oauthSentinelHash placeholder, and for accounts that never set one. The
// change-password flow uses it to decide whether to require the current
// password: OAuth users are *setting* a first password, not changing one.
func (u User) HasPassword() bool {
	return u.PasswordHash != "" && u.PasswordHash != oauthSentinelHash
}

// Age reports how long ago the account was created, relative to now. Drives the
// new-user post / community-create delays (NewUserPostDelay, NewUserCommunityDelay).
func (u User) Age(now time.Time) time.Duration {
	return now.Sub(u.CreatedAt)
}

type Membership struct {
	ID          string
	UserID      string
	CommunityID string
	DisplayName string
	AvatarURL   string
	Role        Role
	TrustLevel  int
	BannedUntil *time.Time
	ApprovedAt  *time.Time
	CreatedAt   time.Time
	// JoinReason is the "why do you want to join?" note a user writes when
	// requesting an approval-gated community. Empty for instant/invited joins.
	JoinReason string
}

func (m Membership) IsBanned(now time.Time) bool {
	return m.BannedUntil != nil && m.BannedUntil.After(now)
}

// IsApproved reports whether an admin has approved this membership. New
// memberships start with approved_at = NULL and stay there until /admin
// approves the join request.
func (m Membership) IsApproved() bool {
	return m.ApprovedAt != nil
}
