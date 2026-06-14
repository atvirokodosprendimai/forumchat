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

// discussionSignals is the datastar bag for the discussions tab + thread page.
type discussionSignals struct {
	Subject     string `json:"projects_discussion_subject"`
	Body        string `json:"projects_discussion_body"`
	Edit        string `json:"projects_discussion_edit"`
	ReplyBody   string `json:"projects_discussion_reply_body"`
	ReplyEdit   string `json:"projects_discussion_reply_edit"`
	QuoteID     string `json:"projects_discussion_quote_id"`
}

// GetDiscussionsTab renders the Discussions list tab.
func (h *Handler) GetDiscussionsTab(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{})
	if !ok {
		return
	}
	pid := chi.URLParam(r, "id")
	threads, err := h.Repo.ListDiscussionThreads(r.Context(), pid)
	if err != nil {
		h.Log.Error("projects discussions list", "err", err, "id", pid)
	}
	id, _ := h.callerIdentity(r)
	isAdmin := id.Role == auth.RoleAdmin
	views := toDiscussionThreadRowViews(threads, id, isAdmin)
	_ = webtempl.ProjectDiscussionsPage(data, views).Render(r.Context(), w)
}

// GetDiscussionThread renders the single-thread view (replies arrive in PD2).
func (h *Handler) GetDiscussionThread(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadProjectData(w, r, struct {
		Todos, Atts, Comments, Activity bool
	}{})
	if !ok {
		return
	}
	pid := chi.URLParam(r, "id")
	did := chi.URLParam(r, "did")
	t, err := h.Repo.DiscussionThreadByID(r.Context(), did)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.Log.Error("projects discussion thread load", "err", err, "id", did)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if t.ProjectID != pid || t.IsDeleted() {
		http.NotFound(w, r)
		return
	}
	id, _ := h.callerIdentity(r)
	isAdmin := id.Role == auth.RoleAdmin
	view := toDiscussionThreadView(t, id, isAdmin)

	replies, err := h.Repo.ListDiscussionReplies(r.Context(), did)
	if err != nil {
		h.Log.Warn("projects discussion replies", "err", err, "thread", did)
	}
	replyViews := toDiscussionReplyViews(replies, id, isAdmin, h.Svc.EditGrace)
	_ = webtempl.ProjectDiscussionThreadPage(data, view, replyViews).Render(r.Context(), w)
}

// PostDiscussionReply adds a reply to a thread.
func (h *Handler) PostDiscussionReply(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	did := chi.URLParam(r, "did")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in discussionSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.Svc.AddDiscussionReply(r.Context(), pid, did, in.QuoteID, in.ReplyBody, id); err != nil {
		if errors.Is(err, ErrEmptyTitle) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Log.Warn("projects discussion reply add", "err", err, "thread", did)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions/" + did)
}

// PostDiscussionReplyEdit replaces a reply body within the grace window.
func (h *Handler) PostDiscussionReplyEdit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	did := chi.URLParam(r, "did")
	rid := chi.URLParam(r, "rid")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in discussionSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.UpdateDiscussionReply(r.Context(), pid, did, rid, in.ReplyEdit, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects discussion reply edit", "err", err, "reply", rid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions/" + did)
}

// PostDiscussionReplyDelete soft-deletes a reply.
func (h *Handler) PostDiscussionReplyDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	did := chi.URLParam(r, "did")
	rid := chi.URLParam(r, "rid")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.DeleteDiscussionReply(r.Context(), pid, did, rid, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects discussion reply delete", "err", err, "reply", rid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions/" + did)
}

func toDiscussionReplyViews(replies []DiscussionReply, viewer Identity, viewerIsAdmin bool, grace time.Duration) []webtempl.ProjectDiscussionReplyView {
	// Build a lookup so we can render a quoted snippet inline.
	byID := map[string]DiscussionReply{}
	for _, rr := range replies {
		byID[rr.ID] = rr
	}
	out := make([]webtempl.ProjectDiscussionReplyView, 0, len(replies))
	now := time.Now().UTC()
	for _, rr := range replies {
		if rr.IsDeleted() {
			continue
		}
		isAuthor := (viewer.UserID != "" && rr.AuthorUserID == viewer.UserID) ||
			(viewer.GuestID != "" && rr.AuthorGuestID == viewer.GuestID)
		canEdit := viewerIsAdmin || (isAuthor && now.Sub(rr.CreatedAt) <= grace)
		canDelete := viewerIsAdmin || isAuthor
		view := webtempl.ProjectDiscussionReplyView{
			ID:              rr.ID,
			AuthorName:      rr.AuthorName,
			IsGuestAuthored: rr.IsGuestAuthored(),
			BodyMD:          rr.BodyMD,
			BodyHTML:        rr.BodyHTML,
			CreatedAt:       rr.CreatedAt,
			Edited:          rr.EditedAt != nil,
			CanEdit:         canEdit,
			CanDelete:       canDelete,
		}
		if rr.QuotedReplyID != "" {
			if q, ok := byID[rr.QuotedReplyID]; ok && !q.IsDeleted() {
				view.QuotedReplyID = q.ID
				view.QuotedAuthor = q.AuthorName
				snip := q.BodyMD
				if len(snip) > 140 {
					snip = snip[:140] + "…"
				}
				view.QuotedSnippet = snip
			}
		}
		out = append(out, view)
	}
	return out
}

// PostCreateDiscussionThread opens a new thread.
func (h *Handler) PostCreateDiscussionThread(w http.ResponseWriter, r *http.Request) {
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
	var in discussionSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	t, err := h.Svc.CreateDiscussionThread(r.Context(), pid, in.Subject, in.Body, id)
	if err != nil {
		h.Log.Warn("projects discussion create", "err", err, "project", pid)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions/" + t.ID)
}

// PostEditDiscussionThread edits subject + body.
func (h *Handler) PostEditDiscussionThread(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	did := chi.URLParam(r, "did")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in discussionSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.UpdateDiscussionThread(r.Context(), pid, did, in.Subject, in.Edit, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects discussion edit", "err", err, "thread", did)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions/" + did)
}

// PostDeleteDiscussionThread soft-deletes a thread.
func (h *Handler) PostDeleteDiscussionThread(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.projectFromURL(w, r)
	if !ok {
		return
	}
	did := chi.URLParam(r, "did")
	c, _ := community.FromContext(r.Context())
	id, ok := h.callerIdentity(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isAdmin := id.Role == auth.RoleAdmin
	if err := h.Svc.DeleteDiscussionThread(r.Context(), pid, did, id, isAdmin); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Warn("projects discussion delete", "err", err, "thread", did)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/projects/" + pid + "/discussions")
}

func toDiscussionThreadView(t DiscussionThread, viewer Identity, viewerIsAdmin bool) webtempl.ProjectDiscussionThreadView {
	canEdit := viewerIsAdmin ||
		(viewer.UserID != "" && t.CreatorUserID == viewer.UserID) ||
		(viewer.GuestID != "" && t.CreatorGuestID == viewer.GuestID)
	return webtempl.ProjectDiscussionThreadView{
		ID:              t.ID,
		Subject:         t.Subject,
		BodyMD:          t.BodyMD,
		BodyHTML:        t.BodyHTML,
		CreatorName:     t.CreatorName,
		CreatedAt:       t.CreatedAt,
		LastActivityAt:  t.LastActivityAt,
		IsGuestAuthored: t.IsGuestAuthored(),
		CanEdit:         canEdit,
		CanDelete:       canEdit,
	}
}

func toDiscussionThreadRowViews(threads []DiscussionThreadRow, viewer Identity, viewerIsAdmin bool) []webtempl.ProjectDiscussionRowView {
	out := make([]webtempl.ProjectDiscussionRowView, 0, len(threads))
	for _, t := range threads {
		out = append(out, webtempl.ProjectDiscussionRowView{
			ID:              t.ID,
			Subject:         t.Subject,
			CreatorName:     t.CreatorName,
			IsGuestAuthored: t.IsGuestAuthored(),
			LastActivityAt:  t.LastActivityAt,
			ReplyCount:      t.ReplyCount,
		})
	}
	return out
}
