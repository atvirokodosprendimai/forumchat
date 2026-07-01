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

// roleRank orders the community roles for hierarchy checks. Unknown roles
// rank 0 (member) so a corrupted/legacy value never grants extra power.
var roleRank = map[Role]int{RoleMember: 0, RoleMod: 1, RoleAdmin: 2, RoleOwner: 3}

// AtLeast reports whether r holds at least min's power (r >= min).
func (r Role) AtLeast(min Role) bool {
	return roleRank[r] >= roleRank[min]
}

// Outranks reports whether r STRICTLY outranks other (r > other). Moderation
// actions against another member (ban, remove, demote, alias) require the
// actor to outrank the target — equal rank is not enough, so an admin can
// never ban a fellow admin or the owner, and nobody below owner touches the
// owner. This is the one hierarchy rule; don't re-derive it per handler.
func (r Role) Outranks(other Role) bool {
	return roleRank[r] > roleRank[other]
}

// RoleRank exposes a role's numeric rank for UI gating (templates compare the
// viewer's rank against a target's). Server-side authz must use AtLeast /
// Outranks, never raw ranks.
func RoleRank(r Role) int { return roleRank[r] }

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
	return u.PasswordHash != "" && u.PasswordHash != oauthSentinelHash && u.PasswordHash != serviceSentinelHash
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
	// EffectiveDisplayName is the admin-set alias when one exists, else the
	// member's own name. It is populated from the generated column when the
	// membership is loaded (MembershipFor/MembershipByID); use ShownName to
	// read it safely. DisplayName always stays the member's OWN name.
	EffectiveDisplayName string
}

func (m Membership) IsBanned(now time.Time) bool {
	return m.BannedUntil != nil && m.BannedUntil.After(now)
}

// ShownName is the name to display to OTHER users — the admin alias when set,
// otherwise the member's own name. Prefer this over DisplayName anywhere a
// member's name is rendered to someone else (push notifications, presence
// labels, cross-user surfaces). The fallback keeps synthetic memberships that
// don't carry the generated column (e.g. SuperAdminMembership) correct.
func (m Membership) ShownName() string {
	if m.EffectiveDisplayName != "" {
		return m.EffectiveDisplayName
	}
	return m.DisplayName
}

// IsApproved reports whether an admin has approved this membership. New
// memberships start with approved_at = NULL and stay there until /admin
// approves the join request.
func (m Membership) IsApproved() bool {
	return m.ApprovedAt != nil
}
