package auth

import "time"

// SuperAdminSet is the platform super-admin allowlist, built from the
// SUPERADMIN_EMAILS config. Membership grants god-mode across every
// community (see the Loader / RequireRole / RequireMember bypasses and
// internal/superadmin). Lookups are case-insensitive on the normalized
// email, the same normalization used when users are stored.
type SuperAdminSet map[string]bool

// NewSuperAdminSet builds the set from a list of emails, normalizing each
// the same way user emails are stored (lowercased, trimmed). Blank entries
// are skipped, so an empty or whitespace-only env value yields no
// super-admins.
func NewSuperAdminSet(emails []string) SuperAdminSet {
	s := make(SuperAdminSet, len(emails))
	for _, e := range emails {
		if e = normEmail(e); e != "" {
			s[e] = true
		}
	}
	return s
}

// Has reports whether email is a platform super-admin.
func (s SuperAdminSet) Has(email string) bool {
	if len(s) == 0 {
		return false
	}
	return s[normEmail(email)]
}

// SuperAdminMembership is the synthetic, never-persisted membership a
// super-admin carries in communities where they have no real row. Role is
// admin and it reads as approved so every per-community gate passes. It has
// no ID — callers must not write it back to the DB.
func SuperAdminMembership(u User, communityID string) Membership {
	now := time.Now()
	return Membership{
		UserID:      u.ID,
		CommunityID: communityID,
		DisplayName: localPart(u.Email),
		Role:        RoleAdmin,
		ApprovedAt:  &now,
	}
}
