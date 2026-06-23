package dashboard

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// isPostLogin returns true when the visit looks like a fresh login landing —
// no Referer or the Referer points at /login. Clicking the "Communities"
// link from the in-app nav has an in-app Referer that fails both checks,
// so the dashboard renders the list instead of bouncing them back to chat.
func isPostLogin(r *http.Request) bool {
	ref := r.Referer()
	if ref == "" {
		return true
	}
	return strings.HasSuffix(ref, "/login") || strings.Contains(ref, "/login?")
}

type Handler struct {
	Communities *community.Repo
	Log         *slog.Logger
}

// GetIndex is the post-login landing. Lists the user's communities. When the
// user belongs to exactly one community we skip the dashboard and redirect
// straight into it — most users only have one.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		// SAAS mode shows the marketing landing as the public front door;
		// otherwise this is a plain private community — send anonymous
		// visitors straight to sign in.
		if webtempl.SaaSEnabled {
			_ = webtempl.LandingPage().Render(r.Context(), w)
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
		return
	}
	rows, err := h.Communities.ListForUser(r.Context(), id.User.ID)
	if err != nil {
		http.Error(w, "load communities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Auto-redirect only when the visit looks post-login (no Referer, or
	// referer is /login). Clicking the "Communities" link from the nav has
	// the in-app referer set; we want to actually show the list there.
	approved := make([]community.MembershipRow, 0, len(rows))
	for _, row := range rows {
		if row.IsApproved && !row.IsBanned {
			approved = append(approved, row)
		}
	}
	if len(approved) == 1 && isPostLogin(r) {
		http.Redirect(w, r, "/c/"+approved[0].Community.Slug+"/chat", http.StatusSeeOther)
		return
	}
	isGlobalAdmin := string(id.Membership.Role) == "admin"
	v := webtempl.Viewer{
		IsAuthed:    true,
		DisplayName: id.Membership.DisplayName,
		Role:        string(id.Membership.Role),
	}
	cards := make([]webtempl.DashboardCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, webtempl.DashboardCard{
			Slug:       row.Community.Slug,
			Name:       row.Community.Name,
			Role:       row.Role,
			IsApproved: row.IsApproved,
			IsBanned:   row.IsBanned,
		})
	}
	_ = webtempl.Dashboard(v, cards, isGlobalAdmin).Render(r.Context(), w)
}
