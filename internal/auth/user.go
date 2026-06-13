package auth

import "time"

type UserStatus string

const (
	StatusPending  UserStatus = "pending"
	StatusActive   UserStatus = "active"
	StatusDisabled UserStatus = "disabled"
)

type Role string

const (
	RoleMember Role = "member"
	RoleMod    Role = "moderator"
	RoleAdmin  Role = "admin"
)

func (r Role) AtLeast(min Role) bool {
	rank := map[Role]int{RoleMember: 0, RoleMod: 1, RoleAdmin: 2}
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
	ID           string
	UserID       string
	CommunityID  string
	DisplayName  string
	AvatarURL    string
	Role         Role
	TrustLevel   int
	BannedUntil  *time.Time
	CreatedAt    time.Time
}

func (m Membership) IsBanned(now time.Time) bool {
	return m.BannedUntil != nil && m.BannedUntil.After(now)
}
