// Package explore powers /explore — a directory of public communities a
// signed-in user can browse and request membership in. The request becomes
// a pending membership row that the target community's admin reviews from
// /c/{slug}/admin.
package explore

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Communities *community.Repo
	AuthRepo    *auth.Repo
	Sessions    *scs.SessionManager
	Cfg         config.Config
	Log         *slog.Logger
}

func (h *Handler) viewer(r *http.Request) (auth.User, bool) {
	uid := auth.CurrentUserID(r.Context(), h.Sessions)
	if uid == "" {
		return auth.User{}, false
	}
	u, err := h.AuthRepo.UserByID(r.Context(), uid)
	if err != nil {
		return auth.User{}, false
	}
	return u, true
}

func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	u, authed := h.viewer(r)
	uid := ""
	if authed {
		uid = u.ID
	}
	list, err := h.Communities.ListPublic(r.Context(), uid)
	if err != nil {
		h.Log.Error("explore list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := webtempl.Viewer{IsAuthed: authed}
	if authed {
		// Best-effort display name from the most recent membership.
		v.DisplayName = u.Email
		if rows, err := h.Communities.ListForUser(r.Context(), u.ID); err == nil && len(rows) > 0 {
			v.DisplayName = u.Email
		}
	}
	rows := make([]webtempl.ExploreRow, 0, len(list))
	for _, c := range list {
		rows = append(rows, webtempl.ExploreRow{
			Slug:        c.Slug,
			Name:        c.Name,
			MemberCount: c.MemberCount,
			IsMember:    c.IsMember,
			IsPending:   c.IsPending,
		})
	}
	_ = webtempl.ExplorePage(webtempl.ExploreData{Viewer: v, Rows: rows}).Render(r.Context(), w)
}

// PostRequestJoin enrolls the user in a public community as a pending member.
// Admins of that community then review the request from /c/{slug}/admin.
func (h *Handler) PostRequestJoin(w http.ResponseWriter, r *http.Request) {
	u, authed := h.viewer(r)
	if !authed {
		http.Redirect(w, r, "/login?next=/explore", http.StatusSeeOther)
		return
	}
	slug := chi.URLParam(r, "slug")
	c, err := h.Communities.BySlug(r.Context(), slug)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !c.IsPublic {
		http.Error(w, "not a public community", http.StatusForbidden)
		return
	}
	if _, err := h.AuthRepo.MembershipFor(r.Context(), u.ID, c.ID); err == nil {
		// Already a member or pending — just bounce back to explore.
		http.Redirect(w, r, "/explore", http.StatusSeeOther)
		return
	} else if !errors.Is(err, auth.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Join policy decides whether the new membership is admitted straight in
	// (open) or lands in the approval queue (request, the default). The
	// community owner sets it in SaaS; self-hosted falls back to the env
	// auto-approve flag.
	settings, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      u.ID,
		CommunityID: c.ID,
		DisplayName: strings.TrimSpace(strings.Split(u.Email, "@")[0]),
		Role:        auth.RoleMember,
	}
	if community.JoinPolicy(settings, h.Cfg) == "open" {
		now := time.Now()
		m.ApprovedAt = &now // straight in, no /pending
	} else {
		// Approval-gated join: capture the optional "why do you want to join?"
		// note for the admin reviewing the pending queue. Plain HTML form POST,
		// so FormValue (not datastar signals) is correct here. Cap the length so
		// a pasted essay can't bloat the row.
		if reason := strings.TrimSpace(r.FormValue("reason")); reason != "" {
			if len(reason) > 1000 {
				reason = reason[:1000]
			}
			m.JoinReason = reason
		}
	}
	if err := h.AuthRepo.CreateMembership(r.Context(), nil, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/explore", http.StatusSeeOther)
}
