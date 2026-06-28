package projects

import (
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

func TestEffectiveAccess(t *testing.T) {
	const (
		creator = "u-creator"
		member  = "u-member"
		other   = "u-other"
	)
	memberID := func(role auth.Role) Identity {
		return Identity{UserID: member, Name: "Member", Role: role}
	}
	guest := Identity{GuestID: "g-1", Name: "Guest", Role: auth.RoleMember}

	open := Project{CreatorUserID: creator, NeedsPerms: false}
	community := func(def string) Project {
		return Project{CreatorUserID: creator, NeedsPerms: true, Visibility: VisibilityCommunity, MemberAccess: def}
	}
	restricted := Project{CreatorUserID: creator, NeedsPerms: true, Visibility: VisibilityRestricted, MemberAccess: AccessRead}

	tests := []struct {
		name    string
		p       Project
		caller  Identity
		grant   string
		grantOK bool
		want    AccessLevel
	}{
		{"legacy open: member writes", open, memberID(auth.RoleMember), "", false, AccessReadWrite},
		// FIX1 M11: a guest's write tracks the project member default (the
		// RequireWrite blanket guest pass-through was removed). Legacy-open lets
		// members write, so a token-admitted guest writes too — no inversion.
		{"legacy open: guest writes (tracks member default)", open, guest, "", false, AccessReadWrite},
		{"community write default: guest writes", community(AccessWrite), guest, "", false, AccessReadWrite},
		// The inversion fix: on a read-only project a guest is read-only, NOT
		// able to out-write a read-only member.
		{"community read default: guest read-only (no inversion)", community(AccessRead), guest, "", false, AccessReadOnly},

		{"perms: guest still reads (token-admitted)", restricted, guest, "", false, AccessReadOnly},

		{"perms: creator manages", community(AccessRead), Identity{UserID: creator, Role: auth.RoleMember}, "", false, AccessReadWrite},
		{"perms: admin manages", restricted, memberID(auth.RoleAdmin), "", false, AccessReadWrite},
		{"perms: owner manages", restricted, memberID(auth.RoleOwner), "", false, AccessReadWrite},

		{"community read default: member reads", community(AccessRead), memberID(auth.RoleMember), "", false, AccessReadOnly},
		{"community write default: member writes", community(AccessWrite), memberID(auth.RoleMember), "", false, AccessReadWrite},

		{"grant upgrades to write on read-only project", community(AccessRead), memberID(auth.RoleMember), AccessWrite, true, AccessReadWrite},
		{"grant downgrades to read on write project", community(AccessWrite), memberID(auth.RoleMember), AccessRead, true, AccessReadOnly},

		{"restricted: no grant is invisible", restricted, memberID(auth.RoleMember), "", false, AccessNone},
		{"restricted: read grant sees it", restricted, memberID(auth.RoleMember), AccessRead, true, AccessReadOnly},
		{"restricted: write grant writes", restricted, memberID(auth.RoleMember), AccessWrite, true, AccessReadWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveAccess(tt.p, tt.caller, tt.grant, tt.grantOK); got != tt.want {
				t.Errorf("EffectiveAccess = %d, want %d", got, tt.want)
			}
		})
	}
}
