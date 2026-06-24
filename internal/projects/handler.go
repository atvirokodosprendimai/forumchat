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
	Title        string   `json:"projects_title"`
	Description  string   `json:"projects_desc"`
	TodoBody     string   `json:"projects_todo_body"`
	TodoEdit     string   `json:"projects_todo_edit"`
	TodoStatus   string   `json:"projects_todo_status"`
	TodoAssignee string   `json:"projects_todo_assignee"`
	TodoOrder    []string `json:"projects_todo_order"`
	CommentBody  string   `json:"projects_comment_body"`
	CommentEdit  string   `json:"projects_comment_edit"`
	// Permission panel (manager-only).
	NeedsPerms   bool   `json:"projects_needs_perms"`
	Visibility   string `json:"projects_visibility"`
	MemberAccess string `json:"projects_member_access"`
	PermUser     string `json:"projects_perm_user"`
	PermAccess   string `json:"projects_perm_access"`
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
	// The index hides restricted projects the viewer may not see. Identity
	// here is rebound to the slug community by RequireMember, so the role
	// is correct for THIS community.
	id, _ := auth.FromContext(r.Context())
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin) || id.IsSuperAdmin
	active, err := h.Repo.ListVisibleForCommunity(r.Context(), c.ID, id.User.ID, isAdmin, false)
	if err != nil {
		h.Log.Error("projects list active", "err", err, "community", c.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	archived, err := h.Repo.ListVisibleForCommunity(r.Context(), c.ID, id.User.ID, isAdmin, true)
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
	perms := PermOpts{
		NeedsPerms:   r.FormValue("needs_perms") == "on" || r.FormValue("needs_perms") == "true",
		Visibility:   r.FormValue("visibility"),
		MemberAccess: r.FormValue("member_access"),
	}
	p, err := h.Svc.CreateProject(r.Context(), c.ID, id.User.ID, title, desc, perms)
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

	isAdmin := caller.Role.AtLeast(auth.RoleAdmin)
	isGuest := caller.IsGuest()

	// Permission gate. EffectiveAccess is the single read/write authority;
	// no-read 404s so a restricted project is not even discoverable.
	grant, grantOK := h.Repo.MemberAccessFor(r.Context(), p.ID, caller.UserID)
	access := EffectiveAccess(p, caller, grant, grantOK)
	if !access.CanRead() {
		http.NotFound(w, r)
		return webtempl.ProjectPageData{}, false
	}
	canWrite := access.CanWrite()
	canManage := !isGuest && (p.CreatorUserID == caller.UserID || isAdmin)

	v := h.layoutViewer(r)
	v.CommunityName = c.Name
	v.CommunitySlug = c.Slug
	share := webtempl.ProjectShareView{Visible: canManage}
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
			CanDelete:       canManage,
			CanWrite:        canWrite,
			CanManage:       canManage,
			NeedsPerms:      p.NeedsPerms,
			Visibility:      p.Visibility,
			MemberAccess:    p.MemberAccess,
		},
		Share:         share,
		IsGuestViewer: isGuest,
	}
	// ACL editor state — only the manager needs the grant list + roster.
	if canManage {
		if members, err := h.Repo.ListMembers(r.Context(), p.ID); err == nil {
			for _, m := range members {
				data.PermMembers = append(data.PermMembers, webtempl.ProjectMemberACLView{
					UserID: m.UserID, Name: m.Name, Access: m.Access,
				})
			}
		}
		data.PermRoster = h.memberOptions(r.Context(), c.ID)
	}
	if want.Todos {
		todos, err := h.Repo.ListTodos(r.Context(), p.ID)
		if err != nil {
			h.Log.Error("projects todos load", "err", err, "id", pid)
		}
		data.Todos = toTodoViews(todos)
		if !isGuest {
			data.TodoMembers = h.memberOptions(r.Context(), c.ID)
		}
	}
	if want.Atts {
		atts, err := h.Repo.ListAttachments(r.Context(), p.ID)
		if err != nil {
			h.Log.Error("projects attachments load", "err", err, "id", pid)
		}
		data.Attachments = h.toAttachmentViews(atts, p.CreatorUserID, caller.UserID, isAdmin)
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

// PostTodoStatus sets the explicit status (todo|in_progress|done) of one
// todo. Like toggle/delete it returns 204 and lets the per-project SSE
// stream re-render #proj-todos.
func (h *Handler) PostTodoStatus(w http.ResponseWriter, r *http.Request) {
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
	if err := h.Svc.SetTodoStatus(r.Context(), pid, tid, in.TodoStatus); err != nil {
		h.Log.Warn("projects todo status", "err", err, "project", pid, "todo", tid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostTodoAssign assigns a member to a todo (empty value = unassign).
func (h *Handler) PostTodoAssign(w http.ResponseWriter, r *http.Request) {
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
	if err := h.Svc.AssignTodo(r.Context(), pid, tid, in.TodoAssignee); err != nil {
		h.Log.Warn("projects todo assign", "err", err, "project", pid, "todo", tid)
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
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
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
	// Authorize: admin or the uploader, and the attachment must belong to the
	// source project — else a foreign project's attachment could be relocated.
	caller, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	att, err := h.Repo.AttachmentByID(r.Context(), aid)
	if err != nil || att.ProjectID != from.ID {
		http.NotFound(w, r)
		return
	}
	if !(caller.Role.AtLeast(auth.RoleAdmin) || (caller.UserID != "" && att.UploaderID == caller.UserID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	// Authorize like the move/edit paths: author or admin, issue ∈ URL project.
	// Without this any caller could refetch (re-pull email into) any issue by id.
	caller, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	issue, err := h.Repo.IssueByID(r.Context(), iid)
	if err != nil || issue.ProjectID != pid {
		http.NotFound(w, r)
		return
	}
	if !issueEditable(caller, issue) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	// Authorize: only the issue author or an admin may move it (mirrors the
	// UI CanEdit gate), and the issue must belong to the source project — else
	// an unauthenticated caller or a foreign project's issue could be moved.
	caller, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Moving an issue across projects is a member-only action — a share-link
	// guest (even the issue author) must not relocate it (the UI gates the
	// picker to non-guests; enforce it server-side too).
	if caller.UserID == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	issue, err := h.Repo.IssueByID(r.Context(), iid)
	if err != nil || issue.ProjectID != from.ID {
		http.NotFound(w, r)
		return
	}
	if !issueEditable(caller, issue) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
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

// managerFromContext resolves the acting user + whether they hold the
// manage role for THIS community. Member-only routes only; identity is
// already rebound to the slug community by RequireMember.
func (h *Handler) managerFromContext(r *http.Request) (userID string, isAdmin, ok bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		return "", false, false
	}
	return id.User.ID, id.Membership.Role.AtLeast(auth.RoleAdmin), true
}

// PostPerms saves a project's permission model (needs_perms master switch +
// visibility + community member default). Manage-gated in the service; the
// stream re-renders the panel + every viewer's affordances.
func (h *Handler) PostPerms(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	userID, isAdmin, ok := h.managerFromContext(r)
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
	if err := h.Svc.SetPerms(r.Context(), pid, userID, isAdmin, in.NeedsPerms, in.Visibility, in.MemberAccess); err != nil {
		h.permError(w, "set perms", pid, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostPermsMember upserts one per-person ACL grant (read|write).
func (h *Handler) PostPermsMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	userID, isAdmin, ok := h.managerFromContext(r)
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
	access := in.PermAccess
	if access == "" {
		access = AccessRead
	}
	if err := h.Svc.GrantMember(r.Context(), pid, userID, isAdmin, in.PermUser, access); err != nil {
		h.permError(w, "grant member", pid, err)
		return
	}
	// Clear the picker so the panel is ready for the next grant.
	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"projects_perm_user":""}`))
}

// PostPermsMemberDelete revokes one per-person ACL grant.
func (h *Handler) PostPermsMemberDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	userID, isAdmin, ok := h.managerFromContext(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	target := chi.URLParam(r, "uid")
	if err := h.Svc.RevokeMember(r.Context(), pid, userID, isAdmin, target); err != nil {
		h.permError(w, "revoke member", pid, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// permError maps perms-action errors to HTTP status, logging unexpected
// ones. Shared by the three perms handlers.
func (h *Handler) permError(w http.ResponseWriter, op, pid string, err error) {
	switch {
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrInvalidPerms), errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		h.Log.Warn("projects "+op, "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
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
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
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
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
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
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
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
	_, p, access, ok := h.projectAccess(w, r)
	if !ok {
		return
	}
	if !access.CanRead() {
		http.NotFound(w, r) // restricted project — don't stream its fragments
		return
	}
	pid := p.ID
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
	views := toCommentViews(comments, id.User.ID, id.Membership.Role.AtLeast(auth.RoleAdmin), h.Svc.EditGrace, now)
	_ = sse.PatchElementTempl(
		webtempl.ProjectCommentsFragment(c.Slug, pid, views, !h.viewerCanWrite(r, pid)),
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
		webtempl.ProjectAttachmentsFragment(c.Slug, pid, h.toAttachmentViews(atts, p.CreatorUserID, id.User.ID, id.Membership.Role.AtLeast(auth.RoleAdmin)), !h.viewerCanWrite(r, pid)),
		datastar.WithSelector("#proj-attachments"),
		datastar.WithModeOuter(),
	)
}

// toAttachmentViews maps stored attachments to view models. It mints a
// signed, inline-served upload URL per row (via SignedURL → the /uploads
// handler sets Content-Disposition: inline) so the Docs tab can embed
// previewable kinds — image / video / audio / pdf — instead of only
// offering a download. Non-previewable kinds fall back to the download
// chip in the template.
func (h *Handler) toAttachmentViews(atts []Attachment, creatorID, viewerID string, viewerIsAdmin bool) []webtempl.ProjectAttachmentView {
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
			URL:       h.Uploads.SignedURL(a.UploadID, viewerID, 24*time.Hour),
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
		webtempl.ProjectTodosFragment(c.Slug, pid, toTodoViews(todos), h.memberOptions(r.Context(), c.ID), !h.viewerCanWrite(r, pid)),
		datastar.WithSelector("#proj-todos"),
		datastar.WithModeOuter(),
	)
}

// memberOptions builds the assignee dropdown's options from the
// community membership roster. Returns nil when AuthRepo is unwired or
// the lookup fails — the template then renders an unassignable list.
func (h *Handler) memberOptions(ctx context.Context, communityID string) []webtempl.ProjectMemberOption {
	if h.AuthRepo == nil {
		return nil
	}
	members, err := h.AuthRepo.ListMembers(ctx, communityID)
	if err != nil {
		h.Log.Warn("projects member options", "err", err, "community", communityID)
		return nil
	}
	out := make([]webtempl.ProjectMemberOption, 0, len(members))
	for _, m := range members {
		out = append(out, webtempl.ProjectMemberOption{ID: m.UserID, Name: m.DisplayName})
	}
	return out
}

func toTodoViews(ts []Todo) []webtempl.ProjectTodoView {
	out := make([]webtempl.ProjectTodoView, 0, len(ts))
	for _, t := range ts {
		status := t.Status
		if status == "" {
			status = TodoStatusTodo
		}
		v := webtempl.ProjectTodoView{
			ID:           t.ID,
			Body:         t.Body,
			Done:         t.Done,
			Status:       status,
			AssigneeID:   t.AssigneeUserID,
			AssigneeName: t.AssigneeName,
			CreatedLabel: t.CreatedAt.Local().Format("Jan 2"),
		}
		if t.CompletedAt != nil {
			v.CompletedLabel = t.CompletedAt.Local().Format("Jan 2")
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) pushHeader(r *http.Request, sse *datastar.ServerSentEventGenerator, pid string) {
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		return
	}
	id, _ := auth.FromContext(r.Context())
	caller := Identity{UserID: id.User.ID, Role: id.Membership.Role}
	grant, grantOK := h.Repo.MemberAccessFor(r.Context(), p.ID, caller.UserID)
	access := EffectiveAccess(p, caller, grant, grantOK)
	canManage := caller.UserID != "" && (p.CreatorUserID == caller.UserID || caller.Role.AtLeast(auth.RoleAdmin))
	view := webtempl.ProjectView{
		ID:              p.ID,
		Title:           p.Title,
		DescriptionMD:   p.DescriptionMD,
		DescriptionHTML: p.DescriptionHTML,
		IsArchived:      p.IsArchived(),
		CanDelete:       canManage,
		CanWrite:        access.CanWrite(),
		CanManage:       canManage,
		NeedsPerms:      p.NeedsPerms,
		Visibility:      p.Visibility,
		MemberAccess:    p.MemberAccess,
	}
	c, _ := community.FromContext(r.Context())
	_ = sse.PatchElementTempl(
		webtempl.ProjectHeaderFragment(c.Slug, view, !view.CanWrite),
		datastar.WithSelector("#proj-header"),
		datastar.WithModeOuter(),
	)
	h.pushPerms(r, sse, c.Slug, view)
}

// viewerCanWrite computes the SSE viewer's write capability on pid so the
// per-fragment push helpers hide write affordances for read-only members
// live, matching the initial page render. Defaults to false on any error.
func (h *Handler) viewerCanWrite(r *http.Request, pid string) bool {
	p, err := h.Repo.ByID(r.Context(), pid)
	if err != nil {
		return false
	}
	id, _ := auth.FromContext(r.Context())
	caller := Identity{UserID: id.User.ID, Role: id.Membership.Role}
	grant, grantOK := h.Repo.MemberAccessFor(r.Context(), pid, caller.UserID)
	return EffectiveAccess(p, caller, grant, grantOK).CanWrite()
}

// pushPerms re-renders the manager-only permissions panel for THIS viewer.
// Non-managers get an empty panel (the stable id stays so a later promotion
// can morph content in). Driven by header events alongside pushHeader.
func (h *Handler) pushPerms(r *http.Request, sse *datastar.ServerSentEventGenerator, slug string, view webtempl.ProjectView) {
	var members []webtempl.ProjectMemberACLView
	var roster []webtempl.ProjectMemberOption
	if view.CanManage {
		if rows, err := h.Repo.ListMembers(r.Context(), view.ID); err == nil {
			for _, m := range rows {
				members = append(members, webtempl.ProjectMemberACLView{UserID: m.UserID, Name: m.Name, Access: m.Access})
			}
		}
		if c, ok := community.FromContext(r.Context()); ok {
			roster = h.memberOptions(r.Context(), c.ID)
		}
	}
	_ = sse.PatchElementTempl(
		webtempl.ProjectPermsPanel(slug, view, members, roster),
		datastar.WithSelector("#proj-perms"),
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

// projectAccess resolves the caller's effective access to the {id} project
// scoped to the URL community. It is the shared gate behind the SSE stream
// and the RequireWrite middleware — both lean on EffectiveAccess so the
// rule lives in one place. ok=false (response already written) when the
// caller can't be identified or the project isn't in this community; a
// missing project / cross-community access 404s (no existence oracle).
func (h *Handler) projectAccess(w http.ResponseWriter, r *http.Request) (Identity, Project, AccessLevel, bool) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return Identity{}, Project{}, AccessNone, false
	}
	caller, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return Identity{}, Project{}, AccessNone, false
	}
	p, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || p.CommunityID != c.ID {
		http.NotFound(w, r)
		return Identity{}, Project{}, AccessNone, false
	}
	grant, grantOK := h.Repo.MemberAccessFor(r.Context(), p.ID, caller.UserID)
	return caller, p, EffectiveAccess(p, caller, grant, grantOK), true
}

// RequireWrite is middleware that gates project mutations on write access.
// Share-link guests pass through (their issue/comment flows are author-gated
// inside each handler, preserving the existing guest behaviour); authed
// members need EffectiveAccess >= write, else 403. A no-read caller 404s so
// a restricted project stays invisible even on a write attempt.
func (h *Handler) RequireWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, _, access, ok := h.projectAccess(w, r)
		if !ok {
			return
		}
		if !access.CanRead() {
			http.NotFound(w, r)
			return
		}
		if access.CanWrite() || caller.IsGuest() {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "read-only access to this project", http.StatusForbidden)
	})
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
	picks, err := h.Repo.ProjectsForCommunities(r.Context(), cids)
	if err != nil {
		h.Log.Error("global issues: project picker", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	v := h.layoutViewer(r)
	v.IsAdminOfAnyCommunity = true
	_ = webtempl.GlobalIssuesPage(webtempl.GlobalIssuesPageData{
		Viewer:      v,
		Communities: groupCommunityProjects(picks),
		Rows:        toGlobalIssueViews(rows),
	}).Render(r.Context(), w)
}

// groupCommunityProjects collapses the flat (community, project) rows
// from ProjectsForCommunities — already ordered by community — into one
// group per community. Rows with an empty ProjectID (communities with no
// active projects) yield a group with no projects.
func groupCommunityProjects(rows []CommunityProjectRow) []webtempl.GlobalCommunityGroup {
	var groups []webtempl.GlobalCommunityGroup
	idx := make(map[string]int, len(rows))
	for _, r := range rows {
		i, ok := idx[r.CommunityID]
		if !ok {
			groups = append(groups, webtempl.GlobalCommunityGroup{
				CommunitySlug: r.CommunitySlug,
				CommunityName: r.CommunityName,
			})
			i = len(groups) - 1
			idx[r.CommunityID] = i
		}
		if r.ProjectID == "" {
			continue
		}
		groups[i].Projects = append(groups[i].Projects, webtempl.GlobalProjectPick{
			ProjectID:    r.ProjectID,
			ProjectTitle: r.ProjectTitle,
			OpenIssues:   r.OpenIssues,
		})
	}
	return groups
}

// GetGlobalIssuesStream is the SSE that powers the "X new — refresh"
// pill on /issues. Polls max(updated_at) in the viewer's admin set; if
// it climbs, increments $_issues_pending. No fat-morph — the user
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
	_ = sse.PatchSignals([]byte(`{"_issues_pending":0}`))

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
				_ = sse.PatchSignals([]byte(`{"_issues_pending":` + strconv.Itoa(pending) + `}`))
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

// postShareCore handles the per-resource share-to-chat POSTs. sig is the
// signal name the calling surface bound its composer to (unique per
// surface so co-rendered composers don't sync — see ShareToChatRow); we
// read just that field, then clear it and patch its own status div.
func (h *Handler) postShareCore(w http.ResponseWriter, r *http.Request, kind, emoji, title, link, sig string) {
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
	var in map[string]any
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	typed, _ := in[sig].(string)
	msgText := strings.TrimSpace(typed)
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
	h.ChatBus.Broadcast("")

	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"` + sig + `":""}`))
	_ = sse.PatchElementTempl(webtempl.SuccessFragment("share-status-"+sig, "Shared to chat."))
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
	h.postShareCore(w, r, "Project", "📂", p.Title, link, "share_msg_project")
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
	h.postShareCore(w, r, "Issue", "🐞", iss.Title, link, "share_msg_issue")
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
	h.postShareCore(w, r, "Doc", "📝", dis.Subject, link, "share_msg_discussion")
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
