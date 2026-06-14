package projects

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler holds the dependencies for the projects HTTP layer.
type Handler struct {
	Repo *Repo
	Svc  *Service
	Bus  *Bus
	Log  *slog.Logger
}

// projectSignals carries the editable values posted from the project
// page (inline header edits).
type projectSignals struct {
	Title       string `json:"projects_title"`
	Description string `json:"projects_desc"`
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

// PostTitle accepts an inline title edit and propagates via SSE.
func (h *Handler) PostTitle(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Svc.UpdateTitle(r.Context(), pid, in.Title); err != nil {
		h.Log.Warn("projects title update", "err", err, "id", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostDescription accepts an inline description edit (markdown) and
// propagates via SSE.
func (h *Handler) PostDescription(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Svc.UpdateDescription(r.Context(), pid, in.Description); err != nil {
		h.Log.Warn("projects desc update", "err", err, "id", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetStream is the long-lived per-project SSE relay. On every Event the
// handler re-renders the affected fragment with WithModeOuter() so
// morphdom swaps the subtree in place.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	sse := datastar.NewSSE(w, r)
	events, unsub := h.Bus.SubscribeProject(pid)
	defer unsub()

	h.Log.Info("projects stream open", "id", pid)
	defer h.Log.Info("projects stream close", "id", pid)

	// Push the current fragments once on open so a late-joiner re-syncs
	// without waiting for the next event.
	h.pushAll(r, sse, pid)

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_ = sse.PatchSignals([]byte(`{}`))
		case ev := <-events:
			switch ev.Kind {
			case "header":
				h.pushHeader(r, sse, pid)
			case "todos", "attachments", "comments", "archive":
				// Phase 4-6 will pushwhichever fragment matches.
				// For now resync the header to cover archive flips.
				h.pushHeader(r, sse, pid)
			}
		}
	}
}

func (h *Handler) pushAll(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	h.pushHeader(r, sse, pid)
}

func (h *Handler) pushHeader(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		return
	}
	id, _ := auth.FromContext(r.Context())
	view := webtempl.ProjectView{
		ID:              p.ID,
		Title:           p.Title,
		DescriptionMD:   p.DescriptionMD,
		DescriptionHTML: p.DescriptionHTML,
		IsArchived:      p.IsArchived(),
		CanDelete:       id.User.ID != "" && (p.CreatorUserID == id.User.ID || id.Membership.Role == auth.RoleAdmin),
	}
	c, _ := community.FromContext(r.Context())
	_ = sse.PatchElementTempl(
		webtempl.ProjectHeaderFragment(c.Slug, view),
		datastar.WithSelector("#proj-header"),
		datastar.WithModeOuter(),
	)
}

// projectFromURL resolves the {id} param, scopes to the current
// community, and 404s on mismatch. Returns (projectID, ok).
func (h *Handler) projectFromURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return "", false
	}
	pid := chi.URLParam(r, "id")
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return "", false
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	if p.CommunityID != c.ID {
		http.NotFound(w, r)
		return "", false
	}
	return pid, true
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
