package forum

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc           *Service
	Repo          *Repo
	Chat          *chat.Service
	NATS          *nats.Conn
	CommunityID   string
	CommunityName string
	BaseURL       string
	Log           *slog.Logger
}

const ThreadLimit = 50

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ts, err := h.Repo.ListThreads(r.Context(), h.CommunityID, ThreadLimit)
	if err != nil {
		http.Error(w, "load threads: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]webtempl.ThreadRow, 0, len(ts))
	for _, t := range ts {
		if t.IsDeleted() {
			continue
		}
		rows = append(rows, webtempl.ThreadRow{
			ID: t.ID, Subject: t.Subject, AuthorName: t.AuthorName, LastActivityAt: t.LastActivityAt,
		})
	}
	_ = webtempl.ForumIndex(h.viewer(r), rows).Render(r.Context(), w)
}

func (h *Handler) GetNew(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.NewThreadPage(h.viewer(r)).Render(r.Context(), w)
}

type newThreadSignals struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in newThreadSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	subject := strings.TrimSpace(in.Subject)
	body := strings.TrimSpace(in.Body)
	if subject == "" || body == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("thread-error", "Subject and body required"))
		return
	}
	t, err := h.Svc.CreateThread(r.Context(), CreateThreadInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		Subject:      subject,
		BodyMarkdown: body,
	})
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("thread-error", err.Error()))
		return
	}

	if h.Chat != nil {
		link := fmt.Sprintf(`%s/forum/%s`, strings.TrimRight(h.BaseURL, "/"), t.ID)
		announceHTML := fmt.Sprintf(
			`<strong>%s</strong> started thread: <a href="%s">%s</a>`,
			htmlEscape(id.Membership.DisplayName), htmlEscape(link), htmlEscape(t.Subject),
		)
		threadID := t.ID
		_, err := h.Chat.PostSystem(r.Context(), h.CommunityID, announceHTML, chat.KindThreadAnnounce, &threadID)
		if err != nil {
			h.Log.Error("post thread-announce", "err", err)
		} else if h.NATS != nil && h.NATS.IsConnected() {
			// Just ping the chat channel; subscribers refetch the latest 100
			// from the DB (which now includes the thread_announce row).
			_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte("changed"))
		}
	}

	_ = sse.Redirect("/forum/" + t.ID)
}

func (h *Handler) GetThread(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	threadID := chi.URLParam(r, "id")
	t, err := h.Repo.GetThread(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if t.IsDeleted() && !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	posts, err := h.Repo.ListPosts(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	view := webtempl.ThreadView{
		ID: t.ID, Subject: t.Subject, AuthorName: t.AuthorName,
		BodyHTML: t.BodyHTML, CreatedAt: t.CreatedAt,
		CanEdit: t.AuthorID == id.User.ID && now.Sub(t.CreatedAt) <= h.Svc.EditGrace,
		IsMod:   isMod,
	}
	pv := make([]webtempl.PostView, 0, len(posts))
	for _, p := range posts {
		pv = append(pv, webtempl.PostView{
			ID:           p.ID,
			AuthorName:   p.AuthorName,
			QuotedAuthor: p.QuotedAuthor,
			QuotedBody:   p.QuotedBody,
			BodyHTML:     p.BodyHTML,
			CreatedAt:    p.CreatedAt,
			Deleted:      p.IsDeleted(),
			CanEdit:      (p.AuthorID == id.User.ID && now.Sub(p.CreatedAt) <= h.Svc.EditGrace) || isMod,
		})
	}
	_ = webtempl.ThreadPage(h.viewer(r), view, pv).Render(r.Context(), w)
}

type replySignals struct {
	Body         string `json:"body"`
	QuotedPostID string `json:"quoted_post_id"`
}

func (h *Handler) PostReply(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threadID := chi.URLParam(r, "id")
	var in replySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	body := strings.TrimSpace(in.Body)
	if body == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("reply-error", "Reply cannot be empty"))
		return
	}
	var quoted *string
	if q := strings.TrimSpace(in.QuotedPostID); q != "" {
		quoted = &q
	}
	if _, err := h.Svc.CreatePost(r.Context(), CreatePostInput{
		ThreadID: threadID, AuthorID: id.User.ID,
		QuotedPostID: quoted, BodyMarkdown: body,
	}); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("reply-error", err.Error()))
		return
	}
	_ = sse.Redirect("/forum/" + threadID)
}

func (h *Handler) PostDeleteThread(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threadID := chi.URLParam(r, "id")
	t, err := h.Repo.GetThread(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	canDelete := isMod || (t.AuthorID == id.User.ID && time.Since(t.CreatedAt) <= h.Svc.EditGrace)
	if !canDelete {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := h.Repo.SoftDeleteThread(r.Context(), threadID); err != nil && !errors.Is(err, ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum")
}

func (h *Handler) PostDeletePost(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	postID := chi.URLParam(r, "id")
	p, err := h.Repo.GetPost(r.Context(), postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	canDelete := isMod || (p.AuthorID == id.User.ID && time.Since(p.CreatedAt) <= h.Svc.EditGrace)
	if !canDelete {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := h.Repo.SoftDeletePost(r.Context(), postID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum/" + p.ThreadID)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
