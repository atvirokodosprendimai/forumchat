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

func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login?next=/forum", http.StatusSeeOther)
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
	_ = webtempl.ForumIndex(h.CommunityName, id.Membership.DisplayName, rows).Render(r.Context(), w)
}

func (h *Handler) GetNew(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.NewThreadPage("").Render(r.Context(), w)
}

func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	subject := strings.TrimSpace(r.PostFormValue("subject"))
	body := strings.TrimSpace(r.PostFormValue("body"))
	if subject == "" || body == "" {
		_ = webtempl.NewThreadPage("Subject and body required").Render(r.Context(), w)
		return
	}
	t, err := h.Svc.CreateThread(r.Context(), CreateThreadInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		Subject:      subject,
		BodyMarkdown: body,
	})
	if err != nil {
		_ = webtempl.NewThreadPage(err.Error()).Render(r.Context(), w)
		return
	}

	// Phase 6 bridge: post a thread_announce system message to chat.
	if h.Chat != nil {
		link := fmt.Sprintf(`%s/forum/%s`, strings.TrimRight(h.BaseURL, "/"), t.ID)
		announceHTML := fmt.Sprintf(
			`<strong>%s</strong> started thread: <a href="%s">%s</a>`,
			htmlEscape(id.Membership.DisplayName), htmlEscape(link), htmlEscape(t.Subject),
		)
		threadID := t.ID
		sysMsg, err := h.Chat.PostSystem(r.Context(), h.CommunityID, announceHTML, chat.KindThreadAnnounce, &threadID)
		if err != nil {
			h.Log.Error("post thread-announce", "err", err)
		} else if h.NATS != nil && h.NATS.IsConnected() {
			// Publish the rendered fragment to the chat channel so SSE subscribers patch it in.
			fragment := renderSystemFragment(sysMsg.ID, sysMsg.CreatedAt, announceHTML)
			_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte(fragment))
		}
	}

	http.Redirect(w, r, "/forum/"+t.ID, http.StatusSeeOther)
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
	quote := r.URL.Query().Get("quote")
	_ = webtempl.ThreadPage(view, pv, "", quote).Render(r.Context(), w)
}

func (h *Handler) PostReply(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	threadID := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" {
		http.Redirect(w, r, "/forum/"+threadID, http.StatusSeeOther)
		return
	}
	var quoted *string
	if q := r.PostFormValue("quoted_post_id"); q != "" {
		quoted = &q
	}
	if _, err := h.Svc.CreatePost(r.Context(), CreatePostInput{
		ThreadID: threadID, AuthorID: id.User.ID,
		QuotedPostID: quoted, BodyMarkdown: body,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/forum/"+threadID+"#post-bottom", http.StatusSeeOther)
}

func (h *Handler) PostDeleteThread(w http.ResponseWriter, r *http.Request) {
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
	http.Redirect(w, r, "/forum", http.StatusSeeOther)
}

func (h *Handler) PostDeletePost(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	http.Redirect(w, r, "/forum/"+p.ThreadID, http.StatusSeeOther)
}

func renderSystemFragment(id string, ts time.Time, html string) string {
	return fmt.Sprintf(`<article class="bubble system" id="msg-%s" data-id="%s"><div class="muted">%s</div>%s</article>`,
		htmlEscape(id), htmlEscape(id), ts.Local().Format("15:04 Jan 2"), html)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
