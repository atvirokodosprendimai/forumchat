package projects

import (
	"database/sql"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// issueSignals is the datastar bag for the issues tab + issue page.
type issueSignals struct {
	Title  string `json:"projects_issue_title"`
	Body   string `json:"projects_issue_body"`
	Edit   string `json:"projects_issue_edit"`
	Status string `json:"projects_issue_status"`
}

// callerIdentity builds an Identity from the request. Phase 1 only
// handles auth users; Phase 3 extends to share-link guests.
func (h *Handler) callerIdentity(r *http.Request) (Identity, bool) {
	if id, ok := auth.FromContext(r.Context()); ok {
		return Identity{
			UserID: id.User.ID,
			Name:   id.Membership.DisplayName,
			Role:   id.Membership.Role,
		}, true
	}
	return Identity{}, false
}

// GetIssuesTab renders the Issues list tab.
func (h *Handler) GetIssuesTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{})
	if !ok {
		return
	}
	pid := chi.URLParam(r, "id")
	issues, err := h.Repo.ListIssues(r.Context(), pid, true)
	if err != nil {
		h.Log.Error("projects issues list", "err", err, "id", pid)
	}
	id, _ := h.callerIdentity(r)
	isAdmin := id.Role == auth.RoleAdmin
	views := toIssueViews(issues, id, isAdmin)
	_ = webtempl.ProjectIssuesPage(data, views).Render(r.Context(), w)
}

// GetIssue renders the single-issue page.
func (h *Handler) GetIssue(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{})
	if !ok {
		return
	}
	pid := chi.URLParam(r, "id")
	iid := chi.URLParam(r, "iid")
	i, err := h.Repo.IssueByID(r.Context(), iid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.Log.Error("projects issue load", "err", err, "id", iid)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if i.ProjectID != pid {
		http.NotFound(w, r)
		return
	}
	id, _ := h.callerIdentity(r)
	isAdmin := id.Role == auth.RoleAdmin
	view := toIssueView(i, id, isAdmin)
	_ = webtempl.ProjectIssuePage(data, view).Render(r.Context(), w)
}

// PostCreateIssue accepts the new-issue form. Members for now; guests
// open up in Phase 4.
func (h *Handler) PostCreateIssue(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	i, err := h.Svc.CreateIssue(r.Context(), pid, in.Title, in.Body, id)
	if err != nil {
		h.Log.Warn("projects issue create", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + i.ID)
}

// PostIssueStatus moves the workflow forward. Member-only.
func (h *Handler) PostIssueStatus(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Svc.UpdateIssueStatus(r.Context(), pid, iid, in.Status, id); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue status", "err", err, "issue", iid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostIssueDelete removes an issue. Author OR admin (creator-guest can
// delete their own).
func (h *Handler) PostIssueDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.DeleteIssue(r.Context(), pid, iid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue delete", "err", err, "issue", iid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues")
}

// PostIssueEdit replaces title + body in one shot.
func (h *Handler) PostIssueEdit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if in.Title != "" {
		if err := h.Svc.UpdateIssueTitle(r.Context(), pid, iid, in.Title, id, isAdmin); err != nil {
			h.Log.Warn("projects issue title", "err", err, "issue", iid)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if in.Edit != "" {
		if err := h.Svc.UpdateIssueBody(r.Context(), pid, iid, in.Edit, id, isAdmin); err != nil {
			h.Log.Warn("projects issue body", "err", err, "issue", iid)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func toIssueView(i Issue, viewer Identity, viewerIsAdmin bool) webtempl.ProjectIssueView {
	canEdit := viewerIsAdmin ||
		(viewer.UserID != "" && i.CreatorUserID == viewer.UserID) ||
		(viewer.GuestID != "" && i.CreatorGuestID == viewer.GuestID)
	return webtempl.ProjectIssueView{
		ID:             i.ID,
		Title:          i.Title,
		BodyMD:         i.BodyMD,
		BodyHTML:       i.BodyHTML,
		Status:         i.Status,
		CreatorName:    i.CreatorName,
		CreatedAt:      i.CreatedAt,
		IsGuestAuthored: i.IsGuestAuthored(),
		CanEdit:        canEdit,
		CanDelete:      canEdit,
		CanChangeStatus: !viewer.IsGuest(),
	}
}

func toIssueViews(issues []Issue, viewer Identity, viewerIsAdmin bool) []webtempl.ProjectIssueView {
	out := make([]webtempl.ProjectIssueView, 0, len(issues))
	for _, i := range issues {
		out = append(out, toIssueView(i, viewer, viewerIsAdmin))
	}
	return out
}
