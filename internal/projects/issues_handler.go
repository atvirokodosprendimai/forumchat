package projects

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// issueSignals is the datastar bag for the issues tab + issue page.
type issueSignals struct {
	Title       string `json:"projects_issue_title"`
	Body        string `json:"projects_issue_body"`
	Edit        string `json:"projects_issue_edit"`
	Status      string `json:"projects_issue_status"`
	CommentBody string `json:"projects_issue_comment_body"`
	CommentEdit string `json:"projects_issue_comment_edit"`
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

// PostIssueComment adds a comment.
func (h *Handler) PostIssueComment(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.Svc.AddIssueComment(r.Context(), pid, iid, id, in.CommentBody); err != nil {
		if errors.Is(err, ErrEmptyTitle) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Log.Warn("projects issue comment add", "err", err, "issue", iid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"projects_issue_comment_body":""}`))
}

// PostIssueCommentEdit replaces a comment body.
func (h *Handler) PostIssueCommentEdit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	cid := chi.URLParam(r, "cid")
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.UpdateIssueComment(r.Context(), pid, iid, cid, id, isAdmin, in.CommentEdit); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue comment edit", "err", err, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostIssueCommentDelete soft-deletes a comment.
func (h *Handler) PostIssueCommentDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	cid := chi.URLParam(r, "cid")
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.DeleteIssueComment(r.Context(), pid, iid, cid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue comment delete", "err", err, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostIssueAttachmentUpload accepts a multipart image upload, scoped
// to either the issue body (no comment-id form field) or a comment.
func (h *Handler) PostIssueAttachmentUpload(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, h.Uploads.MaxSize*4)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	commentID := r.FormValue("comment_id")
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			h.Log.Warn("projects issue attachment open", "err", err)
			continue
		}
		mime := fh.Header.Get("Content-Type")
		if mime == "" {
			mime = "image/png"
		}
		if _, err := h.Svc.AddIssueAttachment(r.Context(), pid, iid, commentID, c.ID, mime, f, id); err != nil {
			h.Log.Warn("projects issue attachment save", "err", err)
		}
		f.Close()
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostIssueAttachmentDelete removes one attachment.
func (h *Handler) PostIssueAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	iid := chi.URLParam(r, "iid")
	aid := chi.URLParam(r, "aid")
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.DeleteIssueAttachment(r.Context(), pid, iid, aid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue attachment delete", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	comments, err := h.Repo.ListIssueComments(r.Context(), iid)
	if err != nil {
		h.Log.Warn("projects issue comments", "err", err, "issue", iid)
	}
	atts, err := h.Repo.ListIssueAttachments(r.Context(), iid)
	if err != nil {
		h.Log.Warn("projects issue attachments", "err", err, "issue", iid)
	}
	commentViews := toIssueCommentViews(comments, id, isAdmin, h.Svc.EditGrace)
	bodyAtts, commentAtts := splitIssueAttachments(atts)
	_ = webtempl.ProjectIssuePage(data, view, commentViews,
		h.attachmentURLs(r, bodyAtts), commentAttachmentURLs(r, commentAtts, h)).Render(r.Context(), w)
}

func toIssueCommentViews(cs []IssueComment, viewer Identity, viewerIsAdmin bool, grace time.Duration) []webtempl.ProjectIssueCommentView {
	out := make([]webtempl.ProjectIssueCommentView, 0, len(cs))
	now := time.Now().UTC()
	for _, c := range cs {
		if c.IsDeleted() {
			continue
		}
		isAuthor := (viewer.UserID != "" && c.AuthorUserID == viewer.UserID) ||
			(viewer.GuestID != "" && c.AuthorGuestID == viewer.GuestID)
		canEdit := viewerIsAdmin || (isAuthor && now.Sub(c.CreatedAt) <= grace)
		canDelete := viewerIsAdmin || isAuthor
		out = append(out, webtempl.ProjectIssueCommentView{
			ID:              c.ID,
			AuthorName:      c.AuthorName,
			IsGuestAuthored: c.AuthorUserID == "" && c.AuthorGuestID != "",
			BodyMD:          c.BodyMD,
			BodyHTML:        c.BodyHTML,
			CreatedAt:       c.CreatedAt,
			Edited:          c.EditedAt != nil,
			CanEdit:         canEdit,
			CanDelete:       canDelete,
		})
	}
	return out
}

func splitIssueAttachments(atts []IssueAttachment) (body []IssueAttachment, comments map[string][]IssueAttachment) {
	comments = map[string][]IssueAttachment{}
	for _, a := range atts {
		if a.CommentID == "" {
			body = append(body, a)
			continue
		}
		comments[a.CommentID] = append(comments[a.CommentID], a)
	}
	return body, comments
}

// attachmentURLs maps body-attached issue images to viewer-scoped
// signed URLs from uploads.Store.SignedURL.
func (h *Handler) attachmentURLs(r *http.Request, atts []IssueAttachment) []webtempl.ProjectIssueAttachmentView {
	id, _ := h.callerIdentity(r)
	viewerID := id.UserID
	if viewerID == "" {
		viewerID = "guest:" + id.GuestID
	}
	out := make([]webtempl.ProjectIssueAttachmentView, 0, len(atts))
	for _, a := range atts {
		u, err := h.Uploads.Get(r.Context(), a.UploadID)
		if err != nil {
			continue
		}
		out = append(out, webtempl.ProjectIssueAttachmentView{
			ID:           a.ID,
			URL:          h.Uploads.SignedURL(u.ID, viewerID, 24*time.Hour),
			MIME:         u.MIME,
			UploaderName: a.UploaderName,
			CanDelete:    canDeleteIssueAttachment(a, id),
		})
	}
	return out
}

func commentAttachmentURLs(r *http.Request, byComment map[string][]IssueAttachment, h *Handler) map[string][]webtempl.ProjectIssueAttachmentView {
	out := map[string][]webtempl.ProjectIssueAttachmentView{}
	for cid, atts := range byComment {
		out[cid] = h.attachmentURLs(r, atts)
	}
	return out
}

func canDeleteIssueAttachment(a IssueAttachment, viewer Identity) bool {
	if viewer.Role == auth.RoleAdmin {
		return true
	}
	if viewer.UserID != "" && a.UploaderUserID == viewer.UserID {
		return true
	}
	if viewer.GuestID != "" && a.UploaderGuestID == viewer.GuestID {
		return true
	}
	return false
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
