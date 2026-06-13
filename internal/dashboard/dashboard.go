package dashboard

import (
	"log/slog"
	"net/http"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

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
		_ = webtempl.Hello("forumchat").Render(r.Context(), w)
		return
	}
	rows, err := h.Communities.ListForUser(r.Context(), id.User.ID)
	if err != nil {
		http.Error(w, "load communities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	approved := make([]community.MembershipRow, 0, len(rows))
	for _, row := range rows {
		if row.IsApproved && !row.IsBanned {
			approved = append(approved, row)
		}
	}
	if len(approved) == 1 {
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
