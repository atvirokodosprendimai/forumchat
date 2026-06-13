package todos

import (
	"log/slog"
	"net/http"
	"strings"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
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

// PostCreate persists a todo derived from a chat message or forum post. The
// source is encoded in `todo_open_source` as `<kind>:<id>`. For chat we
// snapshot body_md + source_day; for forum_post we also store thread_id.
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
	sse := datastar.NewSSE(w, r)

	source := strings.TrimSpace(in.Source)
	title := strings.TrimSpace(in.Title)
	if source == "" || title == "" {
		return
	}
	kind, srcID, ok := splitSource(source)
	if !ok {
		return
	}

	t := Todo{
		CommunityID: c.ID,
		UserID:      id.User.ID,
		SourceID:    srcID,
		Title:       title,
		Category:    strings.TrimSpace(in.Category),
		Note:        strings.TrimSpace(in.Note),
	}
	switch kind {
	case "chat":
		if h.ChatRepo == nil {
			return
		}
		msg, err := h.ChatRepo.ByID(r.Context(), srcID)
		if err != nil {
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
		t.SourceKind = SourceForumPost
		t.BodySnapshot = p.BodyMarkdown
		t.SourceThreadID = p.ThreadID
	default:
		return
	}

	if _, err := h.Repo.Create(r.Context(), t); err != nil {
		h.Log.Error("todo create", "err", err)
		return
	}
	_ = sse.PatchSignals([]byte(`{"todo_open_source":"","todo_title":"","todo_category":"","todo_note":""}`))
}

func splitSource(s string) (kind, id string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
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
