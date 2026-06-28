package projects

import "github.com/atvirokodosprendimai/forumchat/internal/auth"

// AccessLevel is a caller's effective capability on one project.
type AccessLevel int

const (
	AccessNone      AccessLevel = iota // cannot see the project
	AccessReadOnly                     // may read, may not mutate
	AccessReadWrite                    // may read and mutate
)

// CanRead reports whether the level permits viewing the project.
func (l AccessLevel) CanRead() bool { return l >= AccessReadOnly }

// CanWrite reports whether the level permits mutating the project.
func (l AccessLevel) CanWrite() bool { return l >= AccessReadWrite }

// EffectiveAccess resolves what caller may do on p. grant/grantOK come from
// the project_members ACL — grant is AccessRead/AccessWrite and grantOK is
// false when the caller has no row. It is the single source of truth for
// both the read gate (loadProjectData) and the write gate (RequireWrite),
// so the rules live in exactly one pure, table-tested place.
func EffectiveAccess(p Project, caller Identity, grant string, grantOK bool) AccessLevel {
	// Share-link guests are admitted to exactly this project by token, so they
	// read regardless of the perms model. Their WRITE capability tracks the
	// project's member default so a guest never out-ranks a read-only member
	// (FIX1 M11 privilege inversion): a legacy-open project or one whose
	// MemberAccess is write lets guests write; anything else is read-only.
	if caller.IsGuest() {
		if !p.NeedsPerms || p.MemberAccess == AccessWrite {
			return AccessReadWrite
		}
		return AccessReadOnly
	}
	// Legacy open project: every approved member reads + writes, byte-for-
	// byte as before this feature.
	if !p.NeedsPerms {
		return AccessReadWrite
	}
	// Creator + community admin/owner always manage (read + write).
	if caller.UserID != "" && (caller.UserID == p.CreatorUserID || caller.Role.AtLeast(auth.RoleAdmin)) {
		return AccessReadWrite
	}
	// An explicit per-person grant wins over the community default — this is
	// how you upgrade one member to write on an otherwise read-only project,
	// or admit one member to a restricted project.
	if grantOK {
		if grant == AccessWrite {
			return AccessReadWrite
		}
		return AccessReadOnly
	}
	// No grant: community-visible projects fall back to the member default;
	// restricted projects are invisible to everyone else.
	if p.Visibility == VisibilityCommunity {
		if p.MemberAccess == AccessWrite {
			return AccessReadWrite
		}
		return AccessReadOnly
	}
	return AccessNone
}
