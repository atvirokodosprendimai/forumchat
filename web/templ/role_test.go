package templ

import "testing"

// TestRoleHelpers guards the self-host regression fix: migration 00055 relabels
// the admin → owner, so every UI power gate must treat owner as ≥ admin or the
// promoted owner loses the Admin link, moderation menu, etc.
func TestRoleHelpers(t *testing.T) {
	if !RoleIsAdmin("owner") {
		t.Fatal("owner must satisfy RoleIsAdmin (else loses Admin nav, chat moderation, ...)")
	}
	if !RoleIsAdmin("admin") {
		t.Fatal("admin must satisfy RoleIsAdmin")
	}
	if RoleIsAdmin("moderator") || RoleIsAdmin("member") {
		t.Fatal("mod/member must NOT satisfy RoleIsAdmin")
	}
	if !RoleIsMod("owner") || !RoleIsMod("admin") || !RoleIsMod("moderator") {
		t.Fatal("owner/admin/moderator must satisfy RoleIsMod")
	}
	if RoleIsMod("member") {
		t.Fatal("member must NOT satisfy RoleIsMod")
	}
}
