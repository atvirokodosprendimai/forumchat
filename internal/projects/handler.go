package projects

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// commLookupFn resolves a community ID to a community record. Injected
// from main.go so the projects package doesn't need to depend on the
// community Repo type directly.
type commLookupFn func(ctx context.Context, id string) (*CommunityRef, error)

// CommunityRef is the slim community shape projects needs (just the
// slug for redirect URLs).
type CommunityRef struct {
	ID   string
	Slug string
	Name string
}

// Handler holds the dependencies for the projects HTTP layer.
type Handler struct {
	Repo     *Repo
	Svc      *Service
	Bus      *Bus
	Uploads  *uploads.Store
	Sessions *scs.SessionManager // for share-link guest sessions
	// AuthRepo is used by the global /issues page to resolve which
	// communities the viewer admins. Optional — when nil the global
	// route returns 404 (mirrors mailbox.Handler.GetGlobalInbox).
	AuthRepo *auth.Repo
	// PushNotify dispatches a web-push notification. Optional. Wired in
	// main.go to the push package's Sender. Used to broadcast new-project,
	// new-issue and new-comment events to community subscribers.
	PushNotify func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string)
	// RefetchEmailFn powers the "Refetch from email" button on auto-
	// created issues. nil → button is hidden + endpoint returns 503.
	// Wired in main.go to mailbox.Service.RefetchIssueFromEmail.
	RefetchEmailFn func(ctx context.Context, issueID string) (bodyUpdated bool, attached int, err error)
	// ChatRepo + ChatBus power the "Share to chat" buttons on project,
	// issue and discussion pages. Optional — when nil the share endpoint
	// returns 503 and the templates can still render the buttons.
	ChatRepo   *chat.Repo
	ChatBus    *chat.Bus
	Log        *slog.Logger
	commLookup commLookupFn // injected by main.go for the guest-bounce route
}

// SetCommunityLookup injects the community-ID-to-slug resolver. Called
// from main.go where the community.Repo is available.
func (h *Handler) SetCommunityLookup(fn commLookupFn) { h.commLookup = fn }

// projectSignals carries the editable values posted from the project
// page. One bag for everything so we don't need a struct per endpoint.
type projectSignals struct {
	Title       string   `json:"projects_title"`
	Description string   `json:"projects_desc"`
	TodoBody    string   `json:"projects_todo_body"`
	TodoEdit    string   `json:"projects_todo_edit"`
	TodoOrder   []string `json:"projects_todo_order"`
	CommentBody string   `json:"projects_comment_body"`
	CommentEdit string   `json:"projects_comment_edit"`
}

// shareChatSignals is read on the per-resource share-to-chat POSTs.
// One shared field name (`share_message`) is enough because each page
// renders its own composer with its own POST URL — the form below the
// project / issue / discussion just sends what the user typed.
type shareChatSignals struct {
	Message string `json:"share_message"`
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
	if h.PushNotify != nil {
		title := "New project: " + p.Title
		body := id.Membership.DisplayName + " created a new project."
		projectURL := "/c/" + c.Slug + "/projects/" + p.ID
		cid := c.ID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h.PushNotify(ctx, cid, "project_new", nil, title, body, projectURL)
		}()
	}
	http.Redirect(w, r, "/c/"+c.Slug+"/projects/"+p.ID, http.StatusSeeOther)
}

// loadProjectData resolves the project + (selectively) child rows so
// each tab handler only fetches what it actually renders. Returns the
// pre-built ProjectPageData. Honours both auth members AND share-link
// guests.
func (h *Handler) loadProjectData(w http.ResponseWriter, r *http.Request, want struct {
	Todos, Atts, Comments, Activity bool
}) (webtempl.ProjectPageData, bool) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return webtempl.ProjectPageData{}, false
	}
	caller, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return webtempl.ProjectPageData{}, false
	}
	pid := chi.URLParam(r, "id")
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return webtempl.ProjectPageData{}, false
		}
		h.Log.Error("projects byid", "err", err, "id", pid)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return webtempl.ProjectPageData{}, false
	}
	if p.CommunityID != c.ID {
		http.NotFound(w, r)
		return webtempl.ProjectPageData{}, false
	}

	isAdmin := caller.Role == auth.RoleAdmin
	isGuest := caller.IsGuest()
	v := h.layoutViewer(r)
	v.CommunityName = c.Name
	v.CommunitySlug = c.Slug
	share := webtempl.ProjectShareView{Visible: !isGuest && (p.CreatorUserID == caller.UserID || isAdmin)}
	if share.Visible {
		if inv, err := h.Repo.ActiveGuestInviteForProject(r.Context(), p.ID); err == nil {
			scheme, host := publicSchemeHost(r)
			share.URL = scheme + "://" + host + "/projects/share/" + inv.Token
			if inv.ExpiresAt != nil {
				share.HasExpiry = true
				share.ExpiresAt = *inv.ExpiresAt
			}
		}
	}
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
			CanDelete:       !isGuest && (p.CreatorUserID == caller.UserID || isAdmin),
		},
		Share:         share,
		IsGuestViewer: isGuest,
	}
	if want.Todos {
		todos, err := h.Repo.ListTodos(r.Context(), p.ID)
		if err != nil {
			h.Log.Error("projects todos load", "err", err, "id", pid)
		}
		data.Todos = toTodoViews(todos)
	}
	if want.Atts {
		atts, err := h.Repo.ListAttachments(r.Context(), p.ID)
		if err != nil {
			h.Log.Error("projects attachments load", "err", err, "id", pid)
		}
		data.Attachments = toAttachmentViews(atts, p.CreatorUserID, caller.UserID, isAdmin)
	}
	// MovePeers feeds the per-attachment + per-issue "Move to project"
	// pickers. Built once per page render — every active project in
	// this community minus the current one.
	if !isGuest {
		peers, err := h.Repo.ListActiveForCommunity(r.Context(), p.CommunityID)
		if err == nil {
			for _, peer := range peers {
				if peer.ID == p.ID {
					continue
				}
				data.MovePeers = append(data.MovePeers, webtempl.ProjectMovePeer{ID: peer.ID, Title: peer.Title})
			}
			// Also surface peers on every attachment row so the Docs
			// template can render the inline picker without knowing
			// the parent project's peer list.
			for i := range data.Attachments {
				data.Attachments[i].MovePeers = data.MovePeers
			}
		}
	}
	if want.Comments {
		comments, err := h.Repo.ListComments(r.Context(), p.ID)
		if err != nil {
			h.Log.Error("projects comments load", "err", err, "id", pid)
		}
		data.Comments = toCommentViews(comments, caller.UserID, isAdmin, h.Svc.EditGrace, time.Now().UTC())
	}
	if want.Activity {
		activity, err := h.Repo.RecentActivity(r.Context(), p.ID, 30)
		if err != nil {
			h.Log.Error("projects activity load", "err", err, "id", pid)
		}
		data.Activity = toActivityViews(activity)
	}
	return data, true
}

// GetOverview renders the Overview tab (the default landing page for
// a project). Loads todos/attachments/comments only for the count pills.
func (h *Handler) GetOverview(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{Todos: true, Atts: true, Comments: true})
	if !ok {
		return
	}
	_ = webtempl.ProjectOverviewPage(data).Render(r.Context(), w)
}

// GetTodosTab renders the Todos tab.
func (h *Handler) GetTodosTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{Todos: true})
	if !ok {
		return
	}
	_ = webtempl.ProjectTodosPage(data).Render(r.Context(), w)
}

// GetDocsTab renders the Docs (attachments) tab.
func (h *Handler) GetDocsTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{Atts: true})
	if !ok {
		return
	}
	_ = webtempl.ProjectDocsPage(data).Render(r.Context(), w)
}

// GetCommentsTab renders the project-wide Comments tab.
func (h *Handler) GetCommentsTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{Comments: true})
	if !ok {
		return
	}
	_ = webtempl.ProjectCommentsPage(data).Render(r.Context(), w)
}

// GetActivityTab renders the Activity tab.
func (h *Handler) GetActivityTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{Activity: true})
	if !ok {
		return
	}
	_ = webtempl.ProjectActivityPage(data).Render(r.Context(), w)
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
	sse := render.NewSSE(w, r)
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
	category := strings.TrimSpace(r.FormValue("category"))
	if category == "" {
		category = "common"
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
		if _, err := h.Svc.AddAttachment(r.Context(), pid, c.ID, id.User.ID, mime, fh.Filename, category, f); err != nil {
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

// moveTargetSignals reads $mv_target_project from the inbox-style
// shared signal bag. Both the attachment and the issue Move handlers
// share this struct because the form has one select bound to that
// signal and the row identity travels in the URL.
type moveTargetSignals struct {
	TargetProjectID string `json:"mv_target_project"`
}

// PostAttachmentMove re-parents one project_attachments row to a
// different project in the SAME community. Caller must be an approved
// member; admin/creator constraints aren't enforced here (anyone who
// can upload can rearrange — matches the existing Docs UX).
func (h *Handler) PostAttachmentMove(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	aid := chi.URLParam(r, "aid")
	if aid == "" {
		http.Error(w, "missing attachment id", http.StatusBadRequest)
		return
	}
	var in moveTargetSignals
	if err := datastar.ReadSignals(r, &in); err != nil || in.TargetProjectID == "" {
		http.Error(w, "missing mv_target_project", http.StatusBadRequest)
		return
	}
	from, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	to, err := h.Repo.ByID(r.Context(), in.TargetProjectID)
	if err != nil {
		http.Error(w, "target project not found", http.StatusBadRequest)
		return
	}
	if to.CommunityID != from.CommunityID {
		http.Error(w, "target project is in a different community", http.StatusBadRequest)
		return
	}
	if err := h.Repo.MoveAttachmentToProject(r.Context(), aid, to.ID); err != nil {
		h.Log.Warn("projects attachment move", "err", err, "from", pid, "to", to.ID, "aid", aid)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Bus.PublishProject(from.ID, Event{Kind: "attachments"})
	h.Bus.PublishProject(to.ID, Event{Kind: "attachments"})
	w.WriteHeader(http.StatusNoContent)
}

// PostIssueRefetch re-runs the email→issue pipeline against the source
// email of an auto-created issue. Overwrites the issue body with freshly
// decoded text and appends attachments that aren't already present.
// 503 when mailbox refetch isn't wired or this issue wasn't created
// from an email.
func (h *Handler) PostIssueRefetch(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	if iid == "" {
		http.Error(w, "missing issue id", http.StatusBadRequest)
		return
	}
	if h.RefetchEmailFn == nil {
		http.Error(w, "refetch not enabled", http.StatusServiceUnavailable)
		return
	}
	bodyUpdated, attached, err := h.RefetchEmailFn(r.Context(), iid)
	if err != nil {
		h.Log.Warn("issue refetch", "err", err, "issue", iid)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Bus.PublishProject(pid, Event{Kind: "issues"})
	h.Log.Info("issue refetch ok", "issue", iid, "body_updated", bodyUpdated, "attached", attached)
	slug := chi.URLParam(r, "slug")
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + slug + "/projects/" + pid + "/issues/" + iid)
}

// PostIssueMove re-parents one project_issues row to a different
// project in the SAME community.
func (h *Handler) PostIssueMove(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	if iid == "" {
		http.Error(w, "missing issue id", http.StatusBadRequest)
		return
	}
	var in moveTargetSignals
	if err := datastar.ReadSignals(r, &in); err != nil || in.TargetProjectID == "" {
		http.Error(w, "missing mv_target_project", http.StatusBadRequest)
		return
	}
	from, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	to, err := h.Repo.ByID(r.Context(), in.TargetProjectID)
	if err != nil {
		http.Error(w, "target project not found", http.StatusBadRequest)
		return
	}
	if to.CommunityID != from.CommunityID {
		http.Error(w, "target project is in a different community", http.StatusBadRequest)
		return
	}
	if err := h.Repo.MoveIssueToProject(r.Context(), iid, to.ID); err != nil {
		h.Log.Warn("projects issue move", "err", err, "from", pid, "to", to.ID, "iid", iid)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Bus.PublishProject(from.ID, Event{Kind: "issues"})
	h.Bus.PublishProject(to.ID, Event{Kind: "issues"})
	// Issue URL contains the project id, so redirect the caller to the
	// new canonical location. Slug stays the same (same community).
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + slug + "/projects/" + to.ID + "/issues/" + iid)
}

// PostArchive toggles archived_at -> now.
func (h *Handler) PostArchive(w http.ResponseWriter, r *http.Request) {
	h.archiveOrUnarchive(w, r, true)
}

// PostUnarchive clears archived_at.
func (h *Handler) PostUnarchive(w http.ResponseWriter, r *http.Request) {
	h.archiveOrUnarchive(w, r, false)
}

func (h *Handler) archiveOrUnarchive(w http.ResponseWriter, r *http.Request, archive bool) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Membership.Role == auth.RoleAdmin
	var err error
	if archive {
		err = h.Svc.Archive(r.Context(), pid, id.User.ID, isAdmin)
	} else {
		err = h.Svc.Unarchive(r.Context(), pid, id.User.ID, isAdmin)
	}
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects archive toggle", "err", err, "project", pid, "archive", archive)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostDeleteProject hard-deletes a project. Redirects to the index via
// SSE redirect so the user lands somewhere sensible.
func (h *Handler) PostDeleteProject(w http.ResponseWriter, r *http.Request) {
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
	isAdmin := id.Membership.Role == auth.RoleAdmin
	if err := h.Svc.DeleteProject(r.Context(), pid, id.User.ID, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects delete", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects")
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
	sse := render.NewSSE(w, r)
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
	sse := render.NewSSE(w, r)
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
				h.pushActivity(r, sse, pid)
			case "todos":
				h.pushTodos(r, sse, pid)
				h.pushActivity(r, sse, pid)
			case "attachments":
				h.pushAttachments(r, sse, pid)
				h.pushActivity(r, sse, pid)
			case "comments":
				h.pushComments(r, sse, pid)
				h.pushActivity(r, sse, pid)
			}
		}
	}
}

func (h *Handler) pushAll(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	h.pushHeader(r, sse, pid)
	h.pushTodos(r, sse, pid)
	h.pushAttachments(r, sse, pid)
	h.pushComments(r, sse, pid)
	h.pushActivity(r, sse, pid)
}

func (h *Handler) pushActivity(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	events, err := h.Repo.RecentActivity(r.Context(), pid, 30)
	if err != nil {
		return
	}
	views := make([]webtempl.ProjectActivityView, 0, len(events))
	for _, e := range events {
		views = append(views, webtempl.ProjectActivityView{
			Kind: e.Kind,
			At:   e.At,
		})
	}
	_ = sse.PatchElementTempl(
		webtempl.ProjectActivityFragment(views),
		datastar.WithSelector("#proj-activity"),
		datastar.WithModeOuter(),
	)
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
		webtempl.ProjectCommentsFragment(c.Slug, pid, views, false),
		datastar.WithSelector("#proj-comments"),
		datastar.WithModeOuter(),
	)
}

func toActivityViews(events []ActivityEvent) []webtempl.ProjectActivityView {
	out := make([]webtempl.ProjectActivityView, 0, len(events))
	for _, e := range events {
		out = append(out, webtempl.ProjectActivityView{Kind: e.Kind, At: e.At})
	}
	return out
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
		webtempl.ProjectAttachmentsFragment(c.Slug, pid, toAttachmentViews(atts, p.CreatorUserID, id.User.ID, id.Membership.Role == auth.RoleAdmin), false),
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
			Category:  a.Category,
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
		webtempl.ProjectTodosFragment(c.Slug, pid, toTodoViews(todos), false),
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
		webtempl.ProjectHeaderFragment(c.Slug, view, false),
		datastar.WithSelector("#proj-header"),
		datastar.WithModeOuter(),
	)
}

// publicSchemeHost returns the canonical scheme + host for building
// share URLs that work behind a reverse proxy.
func publicSchemeHost(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme, host
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

// GetGlobalIssues renders /issues — the cross-community open-issue
// table for any viewer who admins at least one community. Non-admin
// and unauthenticated viewers get a 404 (anti-enumeration).
func (h *Handler) GetGlobalIssues(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || h.AuthRepo == nil {
		http.NotFound(w, r)
		return
	}
	cids, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil {
		h.Log.Error("global issues: admin cids", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if len(cids) == 0 {
		http.NotFound(w, r)
		return
	}
	rows, err := h.Repo.RecentIssuesAcrossCommunities(r.Context(), cids, false, 100)
	if err != nil {
		h.Log.Error("global issues: query", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	v := h.layoutViewer(r)
	v.IsAdminOfAnyCommunity = true
	_ = webtempl.GlobalIssuesPage(webtempl.GlobalIssuesPageData{
		Viewer: v,
		Rows:   toGlobalIssueViews(rows),
	}).Render(r.Context(), w)
}

// GetGlobalIssuesStream is the SSE that powers the "X new — refresh"
// pill on /issues. Polls max(updated_at) in the viewer's admin set; if
// it climbs, increments $issues_pending. No fat-morph — the user
// chooses when to reload.
func (h *Handler) GetGlobalIssuesStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || h.AuthRepo == nil {
		http.NotFound(w, r)
		return
	}
	cids, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || len(cids) == 0 {
		http.NotFound(w, r)
		return
	}

	baseline, err := h.Repo.MaxIssueUpdatedAt(r.Context(), cids)
	if err != nil {
		h.Log.Error("global issues stream: baseline", "err", err)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"issues_pending":0}`))

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	var pending int
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			n, err := h.Repo.CountIssuesUpdatedAfter(r.Context(), cids, baseline)
			if err != nil {
				continue
			}
			if int(n) != pending {
				pending = int(n)
				_ = sse.PatchSignals([]byte(`{"issues_pending":` + strconv.Itoa(pending) + `}`))
			}
		case <-keep.C:
			_ = sse.PatchSignals([]byte(`{}`))
		}
	}
}

func toGlobalIssueViews(rows []GlobalIssueRow) []webtempl.GlobalIssueRow {
	out := make([]webtempl.GlobalIssueRow, len(rows))
	for i, r := range rows {
		out[i] = webtempl.GlobalIssueRow{
			IssueID:       r.IssueID,
			Title:         r.Title,
			Status:        r.Status,
			UpdatedAtUnix: r.UpdatedAt.Unix(),
			ProjectID:     r.ProjectID,
			ProjectTitle:  r.ProjectTitle,
			CommunityID:   r.CommunityID,
			CommunitySlug: r.CommunitySlug,
			CommunityName: r.CommunityName,
		}
	}
	return out
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

// ----- share to chat -----------------------------------------------------
//
// Three endpoints share the same shape: read the project / issue /
// discussion the URL points at, build a chat message that links to it
// (with the user's optional one-liner), insert + broadcast, clear the
// composer signal so the user can immediately type the next update.

func (h *Handler) postShareCore(w http.ResponseWriter, r *http.Request, kind, emoji, title, link string) {
	if h.ChatRepo == nil || h.ChatBus == nil {
		http.Error(w, "chat sharing not available", http.StatusServiceUnavailable)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in shareChatSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	msgText := strings.TrimSpace(in.Message)
	if len(msgText) > 800 {
		msgText = msgText[:800]
	}
	name := htmlEscProj(title)
	href := htmlEscProj(link)
	var bodyHTML, bodyMD string
	if msgText != "" {
		safeText := htmlEscProj(msgText)
		bodyHTML = emoji + " " + safeText + " — " + kind + ` <strong>` + name + `</strong> · ` +
			`<a href="` + href + `" target="_blank" rel="noopener">Open</a>`
		bodyMD = emoji + " " + msgText + " — " + kind + " " + title + " — " + link
	} else {
		bodyHTML = emoji + " " + kind + ` <strong>` + name + `</strong> · ` +
			`<a href="` + href + `" target="_blank" rel="noopener">Open</a>`
		bodyMD = emoji + " " + kind + " " + title + " — " + link
	}
	aid := id.User.ID
	msg := chat.Message{
		ID:           uuid.NewString(),
		CommunityID:  c.ID,
		AuthorID:     &aid,
		Kind:         chat.KindUser,
		BodyMarkdown: bodyMD,
		BodyHTML:     bodyHTML,
		CreatedAt:    time.Now(),
	}
	if err := h.ChatRepo.Insert(r.Context(), msg); err != nil {
		http.Error(w, "post failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.ChatBus.Broadcast()

	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"share_message":""}`))
	_ = sse.PatchElementTempl(webtempl.SuccessFragment("share-status", "Shared to chat."))
}

func (h *Handler) PostShareProjectToChat(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, _ := community.FromContext(r.Context())
	scheme, host := publicSchemeHost(r)
	link := scheme + "://" + host + "/c/" + c.Slug + "/projects/" + p.ID
	h.postShareCore(w, r, "Project", "📂", p.Title, link)
}

func (h *Handler) PostShareIssueToChat(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	issueID := chi.URLParam(r, "iid")
	if issueID == "" {
		http.Error(w, "missing issue id", http.StatusBadRequest)
		return
	}
	iss, err := h.Repo.IssueByID(r.Context(), issueID)
	if err != nil || iss.ProjectID != pid {
		http.NotFound(w, r)
		return
	}
	c, _ := community.FromContext(r.Context())
	scheme, host := publicSchemeHost(r)
	link := scheme + "://" + host + "/c/" + c.Slug + "/projects/" + pid + "/issues/" + iss.ID
	h.postShareCore(w, r, "Issue", "🐞", iss.Title, link)
}

func (h *Handler) PostShareDiscussionToChat(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	disID := chi.URLParam(r, "did")
	if disID == "" {
		http.Error(w, "missing discussion id", http.StatusBadRequest)
		return
	}
	dis, err := h.Repo.DiscussionThreadByID(r.Context(), disID)
	if err != nil || dis.ProjectID != pid {
		http.NotFound(w, r)
		return
	}
	c, _ := community.FromContext(r.Context())
	scheme, host := publicSchemeHost(r)
	link := scheme + "://" + host + "/c/" + c.Slug + "/projects/" + pid + "/discussions/" + dis.ID
	h.postShareCore(w, r, "Doc", "📝", dis.Subject, link)
}

func htmlEscProj(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
