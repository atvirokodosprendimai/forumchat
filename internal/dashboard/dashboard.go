package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
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
	// Auth supplies the owner-count quota gate (CountOwnedByUser). Cfg gates the
	// whole self-serve flow to SaaS. Provision creates the community on a free
	// self-serve create. All three are only needed for the SaaS create/request
	// card; in self-host they are unused.
	Auth      *auth.Repo
	Cfg       config.Config
	Provision *provision.Service
	Log       *slog.Logger
	// createMu serializes the self-serve owner-quota check→create so two
	// concurrent requests with different slugs can't both pass the "owns 0"
	// gate and create two free communities. The quota is a business rule with
	// no backing DB constraint (owning several is legal after approval), so the
	// check-then-act needs a lock. Single process → in-memory mutex suffices.
	createMu sync.Mutex
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
	_ = webtempl.Dashboard(v, cards, isGlobalAdmin, h.createState(r.Context(), id.User.ID)).Render(r.Context(), w)
}

// createState computes the SaaS self-serve create/request card state for a user:
// may they create a free community, or are they over quota (and possibly already
// have a request pending). Returns a zero (SaaS:false) value in self-host so the
// card is hidden entirely.
func (h *Handler) createState(ctx context.Context, userID string) webtempl.DashboardCreate {
	if !h.Cfg.SAAS {
		return webtempl.DashboardCreate{}
	}
	dc := webtempl.DashboardCreate{SaaS: true}
	owned, err := h.Auth.CountOwnedByUser(ctx, userID)
	if err != nil {
		h.Log.Error("dashboard: count owned communities", "user", userID, "err", err)
		return dc // safest default: no free create, no pending — show the request form
	}
	if owned == 0 {
		dc.CanCreateFree = true
		return dc
	}
	if req, ok, err := h.Communities.PendingRequestForUser(ctx, userID); err == nil && ok {
		dc.HasPending = true
		dc.PendingSlug = req.Slug
	}
	return dc
}
