package projects

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// issueSignals is the datastar bag for the issues tab + issue page.
type issueSignals struct {
	Title       string `json:"projects_issue_title"`
	Body        string `json:"projects_issue_body"`
	BodyImage   string `json:"projects_issue_body_image"`
	Edit        string `json:"projects_issue_edit"`
	EditImage   string `json:"projects_issue_edit_image"`
	Status      string `json:"projects_issue_status"`
	CommentBody string `json:"projects_issue_comment_body"`
	CommentEdit string `json:"projects_issue_comment_edit"`
}

// callerIdentity builds an Identity from the request. Auth users win
// first; otherwise a share-link guest session is honoured, but only
// for the project ID the guest was admitted to.
func (h *Handler) callerIdentity(r *http.Request) (Identity, bool) {
	if id, ok := auth.FromContext(r.Context()); ok {
		// Re-resolve the membership against the URL-slug community. The
		// identity in context was bound by auth.Loader to the SESSION
		// community, which may differ from the community in the URL — this
		// route group has no RequireMember to rebind it. Trusting the
		// session role here let an admin of community B act on community A
		// with admin rights (cross-tenant escalation). Resolve the role for
		// THIS community; a non-member auth user falls through to the
		// share-guest path below (and is denied if not a guest of it).
		if c, ok := community.FromContext(r.Context()); ok && h.AuthRepo != nil {
			m, err := h.AuthRepo.MembershipFor(r.Context(), id.User.ID, c.ID)
			switch {
			case err == nil:
				return Identity{UserID: id.User.ID, Name: m.DisplayName, Role: m.Role}, true
			case id.IsSuperAdmin && errors.Is(err, auth.ErrNotFound):
				m = auth.SuperAdminMembership(id.User, c.ID)
				return Identity{UserID: id.User.ID, Name: m.DisplayName, Role: m.Role}, true
			}
		}
	}
	return h.guestIdentity(r)
}

// issueEditable reports whether caller may edit/move/refetch the issue: its
// author (auth user or share guest) or an admin. Mirrors the UI CanEdit gate.
func issueEditable(caller Identity, i Issue) bool {
	if caller.Role.AtLeast(auth.RoleAdmin) {
		return true
	}
	return (caller.UserID != "" && i.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && i.CreatorGuestID == caller.GuestID)
}

// guestIdentity resolves a share-link guest session, scoped to the one
// project the guest was admitted to. Returns false for anyone who is
// neither an admitted guest of the URL project nor (per callerIdentity) a
// member of the URL community.
func (h *Handler) guestIdentity(r *http.Request) (Identity, bool) {
	if h.Sessions == nil {
		return Identity{}, false
	}
	gid := h.Sessions.GetString(r.Context(), sessKeyProjectGuestID)
	gpid := h.Sessions.GetString(r.Context(), sessKeyProjectGuestProjectID)
	pid := chi.URLParam(r, "id")
	if gid != "" && gpid == pid {
		name := h.Sessions.GetString(r.Context(), sessKeyProjectGuestName)
		return Identity{GuestID: gid, Name: name, Role: auth.RoleMember}, true
	}
	return Identity{}, false
}

// PostShareMint creates / rotates a guest share token for a project.
type shareSignals struct {
	TTL string `json:"projects_share_ttl"`
}

func (h *Handler) PostShareMint(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in shareSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	ttl, err := ParseGuestTTL(in.TTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Membership.Role.AtLeast(auth.RoleAdmin)
	if _, err := h.Svc.MintGuestInvite(r.Context(), pid, id.User.ID, ttl, id.User.ID, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects share mint", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid)
}

// PostShareRevoke cancels the active token.
func (h *Handler) PostShareRevoke(w http.ResponseWriter, r *http.Request) {
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
	if err := h.Svc.RevokeActiveGuestInvite(r.Context(), pid, id.User.ID, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetGuestLanding shows the form for a share-link guest to pick a name.
func (h *Handler) GetGuestLanding(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	inv, err := h.Repo.GuestInviteByToken(r.Context(), token)
	if err != nil || !inv.Active(time.Now().UTC()) {
		http.Error(w, "invite is no longer valid", http.StatusNotFound)
		return
	}
	p, err := h.Repo.ByID(r.Context(), inv.ProjectID)
	if err != nil {
		http.Error(w, "project missing", http.StatusNotFound)
		return
	}
	_ = webtempl.ProjectGuestLandingPage(webtempl.ProjectGuestLandingData{
		Token:       token,
		ProjectName: p.Title,
	}).Render(r.Context(), w)
}

// PostGuestJoin redeems the invite, sets session keys, redirects.
func (h *Handler) PostGuestJoin(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if len(name) > 80 {
		name = name[:80]
	}
	p, gid, err := h.Svc.RedeemGuestInvite(r.Context(), token, name)
	if err != nil {
		_ = webtempl.ProjectGuestLandingPage(webtempl.ProjectGuestLandingData{
			Token: token, Error: err.Error(),
		}).Render(r.Context(), w)
		return
	}
	h.Sessions.Put(r.Context(), sessKeyProjectGuestID, gid.GuestID)
	h.Sessions.Put(r.Context(), sessKeyProjectGuestName, gid.Name)
	h.Sessions.Put(r.Context(), sessKeyProjectGuestProjectID, p.ID)
	http.Redirect(w, r, "/projects/share/"+token+"/go", http.StatusSeeOther)
}

// GetGuestBounce resolves a redeemed token to /c/{slug}/projects/{id}.
// Lets the public-root /projects/share/{token}/join route not need to
// resolve the community slug itself.
func (h *Handler) GetGuestBounce(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	inv, err := h.Repo.GuestInviteByToken(r.Context(), token)
	if err != nil {
		http.Error(w, "invalid invite", http.StatusNotFound)
		return
	}
	p, err := h.Repo.ByID(r.Context(), inv.ProjectID)
	if err != nil {
		http.Error(w, "project missing", http.StatusNotFound)
		return
	}
	// Look up the community slug for the redirect URL.
	if h.commLookup == nil {
		http.Error(w, "community lookup unavailable", http.StatusInternalServerError)
		return
	}
	c, err := h.commLookup(r.Context(), p.CommunityID)
	if err != nil {
		http.Error(w, "community missing", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/c/"+c.Slug+"/projects/"+p.ID, http.StatusSeeOther)
}

// GetIssuesTab renders the Issues list tab. ?status= narrows the list
// to one of {open,triaged,in_progress,closed,all}; default is open.
func (h *Handler) GetIssuesTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{})
	if !ok {
		return
	}
	pid := chi.URLParam(r, "id")
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = IssueOpen
	}
	if !validStatusFilter(status) {
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	issues, err := h.Repo.ListIssues(r.Context(), pid, true, status)
	if err != nil {
		h.Log.Error("projects issues list", "err", err, "id", pid)
	}
	counts, _ := h.Repo.CountIssuesByStatus(r.Context(), pid)
	id, _ := h.callerIdentity(r)
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	views := toIssueViews(issues, id, isAdmin)
	data.IssuesActiveStatus = status
	data.IssuesCounts = counts
	_ = webtempl.ProjectIssuesPage(data, views).Render(r.Context(), w)
}

// PostCloseAllIssues bulk-closes every non-closed issue in the project.
// Admin-edit gated; non-admin members cannot mass-mutate state.
func (h *Handler) PostCloseAllIssues(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	id, ok := h.callerIdentity(r)
	if !ok || !id.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	n, err := h.Repo.CloseAllOpenIssues(r.Context(), pid, time.Now().UTC())
	if err != nil {
		h.Log.Warn("projects close-all", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Bus.PublishProject(pid, Event{Kind: "issues"})
	h.Log.Info("projects close-all done", "project", pid, "closed", n)
	slug := chi.URLParam(r, "slug")
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + slug + "/projects/" + pid + "/issues?status=closed")
}

func validStatusFilter(s string) bool {
	if s == "all" {
		return true
	}
	for _, v := range IssueStatuses {
		if s == v {
			return true
		}
	}
	return false
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
	// Issue page has no per-issue SSE stream — redirect to the same URL
	// via datastar SSE so the page re-renders with the new comment.
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
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
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	if err := h.Svc.UpdateIssueComment(r.Context(), pid, iid, cid, id, isAdmin, in.CommentEdit); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue comment edit", "err", err, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
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
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	if err := h.Svc.DeleteIssueComment(r.Context(), pid, iid, cid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue comment delete", "err", err, "comment", cid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
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
		if _, err := h.Svc.AddIssueAttachment(r.Context(), pid, iid, commentID, c.ID, mime, fh.Filename, f, id); err != nil {
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
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	if err := h.Svc.DeleteIssueAttachment(r.Context(), pid, iid, aid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue attachment delete", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
}

// copyToDocsSignals is the bag read by PostIssueAttachmentCopyToDocs.
// $copy_category + $copy_name are bound to a small per-attachment form.
type copyToDocsSignals struct {
	Category string `json:"copy_category"`
	Name     string `json:"copy_name"`
}

// PostIssueAttachmentCopyToDocs adds a project_attachments row pointing
// at the same upload as the source IssueAttachment, so the Docs tab
// surfaces the file independently of the issue.
func (h *Handler) PostIssueAttachmentCopyToDocs(w http.ResponseWriter, r *http.Request) {
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
	if id.UserID == "" {
		// Guests cannot promote attachments to the project library.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in copyToDocsSignals
	if err := datastar.ReadSignals(r, &in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	if _, err := h.Svc.CopyIssueAttachmentToDocs(r.Context(), pid, iid, aid, id.UserID, in.Category, in.Name); err != nil {
		h.Log.Warn("projects issue attachment copy-to-docs", "err", err, "issue", iid, "att", aid)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
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
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	view := toIssueView(i, id, isAdmin)
	view.BodyHTML = h.resolveUploadURLs(r, view.BodyHTML, id)

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

// resolveUploadURLs replaces upload://<id> placeholder URLs in an HTML
// fragment with viewer-scoped signed URLs. Used to render auto-issue
// bodies that came from emails with inline cid: images.
func (h *Handler) resolveUploadURLs(r *http.Request, htmlIn string, id Identity) string {
	viewerID := id.UserID
	if viewerID == "" {
		viewerID = "guest:" + id.GuestID
	}
	return render.ResolveUploadURLs(htmlIn, func(uploadID string) string {
		return h.Uploads.SignedURL(uploadID, viewerID, 24*time.Hour)
	})
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
	if viewer.Role.AtLeast(auth.RoleAdmin) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := h.composeBodyWithImage(r, c.ID, h.uploaderOwnerID(r, pid, id), in.BodyImage, in.Body)
	if _, err := h.Svc.CreateIssue(r.Context(), pid, in.Title, body, id); err != nil {
		h.Log.Warn("projects issue create", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Land back on the Issues tab (list + fresh create form) so the user can
	// immediately file another. The detail page has no create form, which
	// forced a manual tab-click / refresh to open the next issue.
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues")
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
	c, _ := community.FromContext(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
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
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	if err := h.Svc.DeleteIssue(r.Context(), pid, iid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects issue delete", "err", err, "issue", iid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues")
}

// PostIssueAttachmentDelete trigger reload too — fragments aren't on the
// project SSE stream. (PostIssueAttachmentUpload is fetch-driven, the JS
// already reloads on success.) Moved above PostIssueEdit so the new
// helpers all live in the same neighbourhood.

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
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in issueSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Role.AtLeast(auth.RoleAdmin)
	c, _ := community.FromContext(r.Context())
	if in.Title != "" {
		if err := h.Svc.UpdateIssueTitle(r.Context(), pid, iid, in.Title, id, isAdmin); err != nil {
			h.Log.Warn("projects issue title", "err", err, "issue", iid)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if in.Edit != "" || in.EditImage != "" {
		body := h.composeBodyWithImage(r, c.ID, h.uploaderOwnerID(r, pid, id), in.EditImage, in.Edit)
		if err := h.Svc.UpdateIssueBody(r.Context(), pid, iid, body, id, isAdmin); err != nil {
			h.Log.Warn("projects issue body", "err", err, "issue", iid)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/issues/" + iid)
}

func toIssueView(i Issue, viewer Identity, viewerIsAdmin bool) webtempl.ProjectIssueView {
	canEdit := viewerIsAdmin ||
		(viewer.UserID != "" && i.CreatorUserID == viewer.UserID) ||
		(viewer.GuestID != "" && i.CreatorGuestID == viewer.GuestID)
	return webtempl.ProjectIssueView{
		ID:              i.ID,
		Title:           i.Title,
		BodyMD:          i.BodyMD,
		BodyHTML:        i.BodyHTML,
		Status:          i.Status,
		CreatorName:     i.CreatorName,
		CreatedAt:       i.CreatedAt,
		IsGuestAuthored: i.IsGuestAuthored(),
		CanEdit:         canEdit,
		CanDelete:       canEdit,
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
