package projects

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler holds the dependencies for the projects HTTP layer. Bus gets
// added in Phase 3 when realtime arrives.
type Handler struct {
	Repo *Repo
	Svc  *Service
	Log  *slog.Logger
}

// GetIndex renders /c/{slug}/projects: active projects on top, archived
// collapsed under an expandable section. Empty-state when the community
// has no projects yet.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	active, err := h.Repo.ListActiveForCommunity(r.Context(), c.ID)
	if err != nil {
		h.Log.Error("projects list active", "err", err, "community", c.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	archived, err := h.Repo.ListArchivedForCommunity(r.Context(), c.ID)
	if err != nil {
		h.Log.Error("projects list archived", "err", err, "community", c.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	v := h.layoutViewer(r)
	v.CommunityName = c.Name
	v.CommunitySlug = c.Slug
	data := webtempl.ProjectsGridData{
		Viewer:        v,
		CommunitySlug: c.Slug,
		CommunityName: c.Name,
		Active:        toGridRows(active),
		Archived:      toGridRows(archived),
	}
	_ = webtempl.ProjectsGrid(data).Render(r.Context(), w)
}

// PostCreate accepts a plain HTML form submit from the index page's
// "New project" form. Returns 303 to the new project's page so a
// browser refresh doesn't re-post.
func (h *Handler) PostCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	title := r.FormValue("title")
	desc := r.FormValue("description")
	p, err := h.Svc.CreateProject(r.Context(), c.ID, id.User.ID, title, desc)
	if err != nil {
		if errors.Is(err, ErrEmptyTitle) {
			http.Redirect(w, r, "/c/"+c.Slug+"/projects", http.StatusSeeOther)
			return
		}
		h.Log.Error("projects create", "err", err, "community", c.ID, "user", id.User.ID)
		http.Error(w, "create failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/c/"+c.Slug+"/projects/"+p.ID, http.StatusSeeOther)
}

// GetProject renders the project page with all five panel skeletons.
// Phase 2 only loads the Project row + empty placeholders for the
// realtime panels; they get populated in Phase 4-6.
func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	pid := chi.URLParam(r, "id")
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.Log.Error("projects byid", "err", err, "id", pid)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if p.CommunityID != c.ID {
		// Cross-community lookup — same not-found response so we don't
		// leak project ids across communities.
		http.NotFound(w, r)
		return
	}

	v := h.layoutViewer(r)
	v.CommunityName = c.Name
	v.CommunitySlug = c.Slug
	data := webtempl.ProjectPageData{
		Viewer:        v,
		CommunitySlug: c.Slug,
		CommunityName: c.Name,
		Project: webtempl.ProjectView{
			ID:              p.ID,
			Title:           p.Title,
			DescriptionMD:   p.DescriptionMD,
			DescriptionHTML: p.DescriptionHTML,
			IsArchived:      p.IsArchived(),
			CanDelete:       p.CreatorUserID == id.User.ID || id.Membership.Role == auth.RoleAdmin,
		},
	}
	_ = webtempl.ProjectPage(data).Render(r.Context(), w)
}

func (h *Handler) layoutViewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

func toGridRows(rows []IndexRow) []webtempl.ProjectsGridRow {
	out := make([]webtempl.ProjectsGridRow, 0, len(rows))
	for _, r := range rows {
		preview := r.DescriptionMD
		if len(preview) > 140 {
			preview = preview[:140] + "…"
		}
		out = append(out, webtempl.ProjectsGridRow{
			ID:              r.ID,
			Title:           r.Title,
			Preview:         preview,
			TodoTotal:       r.TodoTotal,
			TodoDone:        r.TodoDone,
			AttachmentCount: r.AttachmentCount,
			CommentCount:    r.CommentCount,
			IsArchived:      r.IsArchived(),
			UpdatedAt:       r.UpdatedAt,
		})
	}
	return out
}
