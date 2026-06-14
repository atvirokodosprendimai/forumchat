package projects

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler holds the dependencies for the projects HTTP layer.
type Handler struct {
	Repo    *Repo
	Svc     *Service
	Bus     *Bus
	Uploads *uploads.Store
	Log     *slog.Logger
}

// projectSignals carries the editable values posted from the project
// page. One bag for everything so we don't need a struct per endpoint.
type projectSignals struct {
	Title         string   `json:"projects_title"`
	Description   string   `json:"projects_desc"`
	TodoBody      string   `json:"projects_todo_body"`
	TodoEdit      string   `json:"projects_todo_edit"`
	TodoOrder     []string `json:"projects_todo_order"`
	CommentBody   string   `json:"projects_comment_body"`
	CommentEdit   string   `json:"projects_comment_edit"`
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

	todos, err := h.Repo.ListTodos(r.Context(), p.ID)
	if err != nil {
		h.Log.Error("projects todos load", "err", err, "id", pid)
	}
	atts, err := h.Repo.ListAttachments(r.Context(), p.ID)
	if err != nil {
		h.Log.Error("projects attachments load", "err", err, "id", pid)
	}
	comments, err := h.Repo.ListComments(r.Context(), p.ID)
	if err != nil {
		h.Log.Error("projects comments load", "err", err, "id", pid)
	}

	v := h.layoutViewer(r)
	v.CommunityName = c.Name
	v.CommunitySlug = c.Slug
	isAdmin := id.Membership.Role == auth.RoleAdmin
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
			CanDelete:       p.CreatorUserID == id.User.ID || isAdmin,
		},
		Todos:       toTodoViews(todos),
		Attachments: toAttachmentViews(atts, p.CreatorUserID, id.User.ID, isAdmin),
		Comments:    toCommentViews(comments, id.User.ID, isAdmin, h.Svc.EditGrace, time.Now().UTC()),
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

// PostTodoAdd appends a checklist row.
func (h *Handler) PostTodoAdd(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.Svc.AddTodo(r.Context(), pid, id.User.ID, in.TodoBody); err != nil {
		h.Log.Warn("projects todo add", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"projects_todo_body":""}`))
}

// PostTodoEdit replaces the body of one todo.
func (h *Handler) PostTodoEdit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	tid := chi.URLParam(r, "tid")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Svc.UpdateTodoBody(r.Context(), pid, tid, in.TodoEdit); err != nil {
		h.Log.Warn("projects todo edit", "err", err, "project", pid, "todo", tid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostTodoToggle flips done.
func (h *Handler) PostTodoToggle(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	tid := chi.URLParam(r, "tid")
	if err := h.Svc.ToggleTodo(r.Context(), pid, tid); err != nil {
		h.Log.Warn("projects todo toggle", "err", err, "project", pid, "todo", tid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostTodoDelete removes one row.
func (h *Handler) PostTodoDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	tid := chi.URLParam(r, "tid")
	if err := h.Svc.DeleteTodo(r.Context(), pid, tid); err != nil {
		h.Log.Warn("projects todo delete", "err", err, "project", pid, "todo", tid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostTodoReorder accepts the new ordering as `projects_todo_order`
// (string array of todo IDs). Client-side drag emits it.
func (h *Handler) PostTodoReorder(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Svc.ReorderTodos(r.Context(), pid, in.TodoOrder); err != nil {
		h.Log.Warn("projects todo reorder", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostAttachmentUpload accepts a multipart upload (one or many files)
// and creates one project_attachments row per file. Returns 204; the
// SSE morph drives the UI update.
func (h *Handler) PostAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	c, _ := community.FromContext(r.Context())
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	// Cap the entire request at MaxSize*8 so a multi-file drop can't
	// blow up memory; each individual file still gets the per-upload
	// MaxSize cap inside SaveAttachment.
	r.Body = http.MaxBytesReader(w, r.Body, h.Uploads.MaxSize*8)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		// Some clients use the singular "file" name; honour that too.
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			h.Log.Warn("projects upload open", "err", err, "name", fh.Filename)
			continue
		}
		mime := fh.Header.Get("Content-Type")
		if mime == "" {
			mime = "application/octet-stream"
		}
		if _, err := h.Svc.AddAttachment(r.Context(), pid, c.ID, id.User.ID, mime, fh.Filename, f); err != nil {
			h.Log.Warn("projects upload save", "err", err, "name", fh.Filename)
		}
		f.Close()
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetAttachmentDownload streams the underlying file with a
// Content-Disposition header so the browser saves it under the
// original filename instead of the on-disk content-hash name.
func (h *Handler) GetAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	aid := chi.URLParam(r, "aid")
	a, err := h.Repo.AttachmentByID(r.Context(), aid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if a.ProjectID != pid {
		http.NotFound(w, r)
		return
	}
	u, err := h.Uploads.Get(r.Context(), a.UploadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := h.Uploads.PathFor(u)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(a.Filename)+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

// PostAttachmentDelete removes an attachment after permission check.
func (h *Handler) PostAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	aid := chi.URLParam(r, "aid")
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Membership.Role == auth.RoleAdmin
	if err := h.Svc.DeleteAttachment(r.Context(), pid, aid, id.User.ID, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects attachment delete", "err", err, "project", pid, "attachment", aid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostComment adds a new comment to the project.
func (h *Handler) PostComment(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.Svc.AddComment(r.Context(), pid, id.User.ID, in.CommentBody); err != nil {
		if errors.Is(err, ErrEmptyTitle) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Log.Warn("projects comment add", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"projects_comment_body":""}`))
}

// PostCommentEdit replaces a comment body within the grace window.
func (h *Handler) PostCommentEdit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	cid := chi.URLParam(r, "cid")
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in projectSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Membership.Role == auth.RoleAdmin
	if err := h.Svc.UpdateComment(r.Context(), pid, cid, id.User.ID, isAdmin, in.CommentEdit); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects comment edit", "err", err, "project", pid, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostCommentDelete soft-deletes a comment.
func (h *Handler) PostCommentDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	cid := chi.URLParam(r, "cid")
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Membership.Role == auth.RoleAdmin
	if err := h.Svc.DeleteComment(r.Context(), pid, cid, id.User.ID, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects comment delete", "err", err, "project", pid, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeFilename drops the four chars that break a quoted
// Content-Disposition value, keeping everything else (Unicode is fine
// inside RFC 6266 quoted-string for modern browsers).
func sanitizeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '"', '\\', '\r', '\n':
			continue
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "file"
	}
	return string(out)
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
			case "header", "archive":
				h.pushHeader(r, sse, pid)
			case "todos":
				h.pushTodos(r, sse, pid)
			case "attachments":
				h.pushAttachments(r, sse, pid)
			case "comments":
				h.pushComments(r, sse, pid)
			}
		}
	}
}

func (h *Handler) pushAll(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	h.pushHeader(r, sse, pid)
	h.pushTodos(r, sse, pid)
	h.pushAttachments(r, sse, pid)
	h.pushComments(r, sse, pid)
}

func (h *Handler) pushComments(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	comments, err := h.Repo.ListComments(r.Context(), pid)
	if err != nil {
		return
	}
	id, _ := auth.FromContext(r.Context())
	c, _ := community.FromContext(r.Context())
	now := time.Now().UTC()
	views := toCommentViews(comments, id.User.ID, id.Membership.Role == auth.RoleAdmin, h.Svc.EditGrace, now)
	_ = sse.PatchElementTempl(
		webtempl.ProjectCommentsFragment(c.Slug, pid, views),
		datastar.WithSelector("#proj-comments"),
		datastar.WithModeOuter(),
	)
}

func toCommentViews(cs []Comment, viewerID string, viewerIsAdmin bool, grace time.Duration, now time.Time) []webtempl.ProjectCommentView {
	out := make([]webtempl.ProjectCommentView, 0, len(cs))
	for _, c := range cs {
		if c.IsDeleted() {
			continue
		}
		isAuthor := c.AuthorID == viewerID
		canEdit := viewerIsAdmin || (isAuthor && now.Sub(c.CreatedAt) <= grace)
		canDelete := viewerIsAdmin || isAuthor
		edited := c.EditedAt != nil
		out = append(out, webtempl.ProjectCommentView{
			ID:        c.ID,
			BodyMD:    c.BodyMD,
			BodyHTML:  c.BodyHTML,
			CreatedAt: c.CreatedAt,
			Edited:    edited,
			CanEdit:   canEdit,
			CanDelete: canDelete,
		})
	}
	return out
}

func (h *Handler) pushAttachments(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	atts, err := h.Repo.ListAttachments(r.Context(), pid)
	if err != nil {
		return
	}
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		return
	}
	id, _ := auth.FromContext(r.Context())
	c, _ := community.FromContext(r.Context())
	_ = sse.PatchElementTempl(
		webtempl.ProjectAttachmentsFragment(c.Slug, pid, toAttachmentViews(atts, p.CreatorUserID, id.User.ID, id.Membership.Role == auth.RoleAdmin)),
		datastar.WithSelector("#proj-attachments"),
		datastar.WithModeOuter(),
	)
}

func toAttachmentViews(atts []Attachment, creatorID, viewerID string, viewerIsAdmin bool) []webtempl.ProjectAttachmentView {
	out := make([]webtempl.ProjectAttachmentView, 0, len(atts))
	for _, a := range atts {
		canDelete := viewerIsAdmin || a.UploaderID == viewerID || creatorID == viewerID
		out = append(out, webtempl.ProjectAttachmentView{
			ID:        a.ID,
			Filename:  a.Filename,
			MIME:      a.MIME,
			SizeBytes: a.SizeBytes,
			CreatedAt: a.CreatedAt,
			CanDelete: canDelete,
		})
	}
	return out
}

func (h *Handler) pushTodos(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	todos, err := h.Repo.ListTodos(r.Context(), pid)
	if err != nil {
		return
	}
	c, _ := community.FromContext(r.Context())
	_ = sse.PatchElementTempl(
		webtempl.ProjectTodosFragment(c.Slug, pid, toTodoViews(todos)),
		datastar.WithSelector("#proj-todos"),
		datastar.WithModeOuter(),
	)
}

func toTodoViews(ts []Todo) []webtempl.ProjectTodoView {
	out := make([]webtempl.ProjectTodoView, 0, len(ts))
	for _, t := range ts {
		out = append(out, webtempl.ProjectTodoView{
			ID:   t.ID,
			Body: t.Body,
			Done: t.Done,
		})
	}
	return out
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
