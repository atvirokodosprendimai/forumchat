package auth

import "time"

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
