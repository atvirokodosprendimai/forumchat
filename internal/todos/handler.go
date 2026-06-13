package todos

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo *Repo
	Log  *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if c, ok := community.FromContext(r.Context()); ok {
		v.CommunityName = c.Name
		v.CommunitySlug = c.Slug
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the todos page for the viewer in the current community.
// Filters: ?status= (active|open|doing|done|all, default active), ?category=
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c := community.MustFromContext(r.Context())

	status := r.URL.Query().Get("status")
	switch status {
	case "open", "doing", "done", "all", "active":
	default:
		status = "active"
	}
	category := strings.TrimSpace(r.URL.Query().Get("category"))

	rows, err := h.Repo.ListForUser(r.Context(), id.User.ID, c.ID, Filter{Status: status, Category: category})
	if err != nil {
		http.Error(w, "load todos: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cats, _ := h.Repo.DistinctCategories(r.Context(), id.User.ID, c.ID)

	views := make([]webtempl.TodoRow, 0, len(rows))
	for _, t := range rows {
		views = append(views, todoToView(t))
	}
	_ = webtempl.TodosPage(webtempl.TodosPageData{
		Viewer:     h.viewer(r),
		Rows:       views,
		Categories: cats,
		Status:     status,
		Category:   category,
	}).Render(r.Context(), w)
}

func todoToView(t Todo) webtempl.TodoRow {
	return webtempl.TodoRow{
		ID:             t.ID,
		Title:          t.Title,
		Category:       t.Category,
		Note:           t.Note,
		Status:         string(t.Status),
		SourceKind:     string(t.SourceKind),
		SourceID:       t.SourceID,
		SourceThreadID: t.SourceThreadID,
		SourceDay:      t.SourceDay,
		CreatedAt:      t.CreatedAt,
	}
}
