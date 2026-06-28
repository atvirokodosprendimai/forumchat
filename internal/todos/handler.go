package todos

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo     *Repo
	ChatRepo *chat.Repo
	Forum    *forum.Repo
	Log      *slog.Logger
}

type createSignals struct {
	Source   string `json:"todo_open_source"`
	Title    string `json:"todo_title"`
	Category string `json:"todo_category"`
	Note     string `json:"todo_note"`
}

// PostCreate persists a todo. The source is encoded in `todo_open_source`:
// "manual" for a standalone todo, or "<kind>:<id>" derived from a chat message
// ("chat") or forum post ("forum_post"). For chat we snapshot body_md +
// source_day; for forum_post we also store thread_id; manual keeps neither.
func (h *Handler) PostCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c := community.MustFromContext(r.Context())

	var in createSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)

	source := strings.TrimSpace(in.Source)
	title := strings.TrimSpace(in.Title)
	if source == "" || title == "" {
		return
	}

	t := Todo{
		CommunityID: c.ID,
		UserID:      id.User.ID,
		Title:       title,
		Category:    strings.TrimSpace(in.Category),
		Note:        strings.TrimSpace(in.Note),
	}

	if source == "manual" {
		t.SourceKind = SourceManual
	} else {
		kind, srcID, ok := splitSource(source)
		if !ok {
			return
		}
		t.SourceID = srcID
		switch kind {
		case "chat":
			if h.ChatRepo == nil {
				return
			}
			msg, err := h.ChatRepo.ByID(r.Context(), srcID)
			// Scope the source to the viewer's community (FIX1 M10): ByID/GetPost
			// look up by raw id, so without this a member could snapshot another
			// tenant's chat message via a crafted todo_open_source.
			if err != nil || msg.CommunityID != c.ID {
				return
			}
			t.SourceKind = SourceChat
			t.BodySnapshot = msg.BodyMarkdown
			t.SourceDay = msg.CreatedAt.Format("2006-01-02")
		case "forum_post":
			if h.Forum == nil {
				return
			}
			p, err := h.Forum.GetPost(r.Context(), srcID)
			if err != nil {
				return
			}
			// A forum post carries no community id; resolve it via its thread and
			// confirm it belongs to the viewer's community (FIX1 M10).
			th, err := h.Forum.GetThread(r.Context(), p.ThreadID)
			if err != nil || th.CommunityID != c.ID {
				return
			}
			t.SourceKind = SourceForumPost
			t.BodySnapshot = p.BodyMarkdown
			t.SourceThreadID = p.ThreadID
		default:
			return
		}
	}

	if _, err := h.Repo.Create(r.Context(), t); err != nil {
		h.Log.Error("todo create", "err", err)
		return
	}
	// Refresh the list for a viewer who's on the todos page; idempotent no-op
	// when added from a chat/forum page (no #todos-list in the DOM).
	h.patchList(sse, r, id.User.ID, c.ID)
	_ = sse.PatchSignals([]byte(`{"todo_open_source":"","todo_title":"","todo_category":"","todo_note":""}`))
}

func splitSource(s string) (kind, id string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// PostStatus cycles a todo to the requested status. Path /c/{slug}/todos/{id}/status?next=<status>
// Accepts open/doing/done. The list is patched in-place via outer-morph so
// the row order updates without a full reload.
func (h *Handler) PostStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c := community.MustFromContext(r.Context())
	todoID := chi.URLParam(r, "id")
	next := Status(strings.TrimSpace(r.URL.Query().Get("next")))
	switch next {
	case StatusOpen, StatusDoing, StatusDone:
	default:
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	if err := h.Repo.UpdateStatus(r.Context(), id.User.ID, todoID, next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.patchList(render.NewSSE(w, r), r, id.User.ID, c.ID)
}

// PostDelete removes the todo and patches the list. Path /c/{slug}/todos/{id}/delete.
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c := community.MustFromContext(r.Context())
	todoID := chi.URLParam(r, "id")
	if err := h.Repo.Delete(r.Context(), id.User.ID, todoID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.patchList(render.NewSSE(w, r), r, id.User.ID, c.ID)
}

// patchList re-renders #todos-list using the current ?status & ?category
// filter (or the defaults), then outer-morphs onto the supplied stream.
func (h *Handler) patchList(sse *datastar.ServerSentEventGenerator, r *http.Request, userID, communityID string) {
	status := r.URL.Query().Get("status")
	switch status {
	case "open", "doing", "done", "all", "active":
	default:
		status = "active"
	}
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	rows, err := h.Repo.ListForUser(r.Context(), userID, communityID, Filter{Status: status, Category: category})
	if err != nil {
		h.Log.Error("todo list", "err", err)
		return
	}
	views := make([]webtempl.TodoRow, 0, len(rows))
	for _, t := range rows {
		views = append(views, todoToView(t))
	}
	c, _ := community.FromContext(r.Context())
	_ = sse.PatchElementTempl(webtempl.TodosList(c.Slug, views), datastar.WithModeOuter())
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
