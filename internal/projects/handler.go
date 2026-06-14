package projects

import (
	"log/slog"
	"net/http"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler holds the dependencies for the projects HTTP layer. Service
// and Bus get added in Phase 3 when realtime arrives.
type Handler struct {
	Repo *Repo
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
