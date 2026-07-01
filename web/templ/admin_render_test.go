package templ

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAdminMembers_HierarchyGatesForms pins the UI half of the moderation
// hierarchy fix: a row the viewer cannot moderate (CanModerate=false — the
// owner, a peer admin, or the viewer themselves) must render NO ban/remove/
// alias affordances, while a moderable row keeps them. The server re-checks
// every endpoint; this stops the UI offering buttons that would only 403.
func TestAdminMembers_HierarchyGatesForms(t *testing.T) {
	t.Parallel()
	rows := []AdminMember{
		{MembershipID: "m-owner", DisplayName: "olga", Role: "owner", CreatedAt: time.Now(), CanModerate: false},
		{MembershipID: "m-member", DisplayName: "bob", Role: "member", CreatedAt: time.Now(), CanModerate: true},
	}
	var sb strings.Builder
	if err := AdminMembers("acme", rows).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	if got := strings.Count(html, "Ban…"); got != 1 {
		t.Fatalf("want exactly 1 Ban… form (moderable row only), got %d", got)
	}
	if got := strings.Count(html, "Remove…"); got != 1 {
		t.Fatalf("want exactly 1 Remove… form, got %d", got)
	}
	if strings.Contains(html, "/admin/ban?id=m-owner") {
		t.Fatal("owner row must not carry a ban action")
	}
	if !strings.Contains(html, "/admin/ban?id=m-member") {
		t.Fatal("member row must keep its ban action")
	}
}
