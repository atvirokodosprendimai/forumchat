package privatemsg

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc      *Service
	Repo     *Repo
	Bus      *Bus
	AuthRepo *auth.Repo
	Sessions *scs.SessionManager
	Log      *slog.Logger
}

// Routes mounts /messages/* under the supplied router. Caller is responsible
// for wrapping in an auth-required middleware (we still re-check the session
// in each handler for safety).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/messages", h.GetInbox)
	r.Get("/messages/badge", h.GetBadge)
	r.Get("/messages/stream", h.GetStream)
	r.Post("/messages/new", h.PostNew)
	r.Get("/messages/{id}", h.GetThread)
	r.Get("/messages/{id}/stream", h.GetThreadStream)
	r.Post("/messages/{id}/send", h.PostSend)
	r.Post("/messages/{id}/accept", h.PostAccept)
	r.Post("/messages/{id}/decline", h.PostDecline)
	r.Post("/messages/{id}/read", h.PostRead)
}

type viewer struct {
	UserID      string
	Email       string
	DisplayName string
}

func (h *Handler) viewer(r *http.Request) (viewer, bool) {
	uid := auth.CurrentUserID(r.Context(), h.Sessions)
	if uid == "" {
		return viewer{}, false
	}
	u, err := h.AuthRepo.UserByID(r.Context(), uid)
	if err != nil {
		return viewer{}, false
	}
	name, _ := h.Repo.DisplayName(r.Context(), uid)
	if name == "" {
		name = u.Email
	}
	return viewer{UserID: u.ID, Email: u.Email, DisplayName: name}, true
}

// layoutViewer builds the topbar Viewer for a global page. We hand the
// existing community context through when present (so per-community nav
// links keep working), otherwise the layout degrades to global-only nav.
func (h *Handler) layoutViewer(r *http.Request, v viewer) webtempl.Viewer {
	out := webtempl.Viewer{
		IsAuthed:    true,
		DisplayName: v.DisplayName,
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		out.Role = string(id.Membership.Role)
	}
	return out
}

func (h *Handler) GetInbox(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Redirect(w, r, "/login?next=/messages", http.StatusSeeOther)
		return
	}
	rows, err := h.inboxRows(r.Context(), u.UserID)
	if err != nil {
		h.Log.Error("pm inbox", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := h.layoutViewer(r, u)
	_ = webtempl.MessagesInbox(webtempl.MessagesInboxData{Viewer: v, Rows: rows}).Render(r.Context(), w)
}

func (h *Handler) inboxRows(ctx context.Context, userID string) ([]webtempl.MessagesInboxRow, error) {
	threads, err := h.Repo.ListThreadsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	unread, err := h.Repo.UnreadByThread(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]webtempl.MessagesInboxRow, 0, len(threads))
	for _, t := range threads {
		otherID := t.Other(userID)
		otherName := h.resolveName(ctx, otherID)
		snippet := ""
		if last, err := h.Repo.LatestMessage(ctx, t.ID); err == nil {
			snippet = trim(last.Body, 120)
		}
		out = append(out, webtempl.MessagesInboxRow{
			ID:            t.ID,
			OtherUserID:   otherID,
			OtherUserName: otherName,
			Status:        string(t.Status),
			LastSnippet:   snippet,
			LastMessageAt: t.LastMessageAt,
			Unread:        unread[t.ID],
			IsIncoming:    t.RecipientUserID == userID,
		})
	}
	return out, nil
}

func (h *Handler) GetThread(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Redirect(w, r, "/login?next=/messages", http.StatusSeeOther)
		return
	}
	id := chi.URLParam(r, "id")
	t, err := h.Repo.ThreadByID(r.Context(), id)
	if err != nil || !t.HasMember(u.UserID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	msgs, err := h.Repo.MessagesByThread(r.Context(), t.ID)
	if err != nil {
		h.Log.Error("pm msgs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Mark read on every page open.
	_ = h.Svc.MarkRead(r.Context(), t.ID, u.UserID)

	otherID := t.Other(u.UserID)
	otherName := h.resolveName(r.Context(), otherID)

	v := h.layoutViewer(r, u)
	_ = webtempl.MessagesThread(webtempl.MessagesThreadData{
		Viewer:        v,
		ThreadID:      t.ID,
		Status:        string(t.Status),
		OtherUserID:   otherID,
		OtherUserName: otherName,
		Messages:      toMsgViews(msgs, u.UserID),
		ViewerIsRecipient: t.RecipientUserID == u.UserID,
		ViewerUserID:  u.UserID,
	}).Render(r.Context(), w)
}

type newSignals struct {
	ToUser         string `json:"pm_to_user"`
	Body           string `json:"pm_body"`
	SourceCommID   string `json:"pm_source_community_id"`
	SourceChatMsg  string `json:"pm_source_chat_id"`
}

func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in newSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	to := strings.TrimSpace(in.ToUser)
	body := strings.TrimSpace(in.Body)
	if to == "" || body == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	t, _, err := h.Svc.CreateRequest(r.Context(), u.UserID, to, body, in.SourceCommID, in.SourceChatMsg)
	if err != nil {
		h.Log.Warn("pm create", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	// Clear compose signals and bounce to the thread.
	_ = sse.PatchSignals([]byte(`{"pm_to_user":"","pm_body":"","pm_open_to_user":"","pm_open_to_name":"","pm_source_community_id":"","pm_source_chat_id":""}`))
	_ = sse.Redirect("/messages/" + t.ID)
}

type sendSignals struct {
	Body string `json:"pm_send_body"`
}

func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	tid := chi.URLParam(r, "id")
	if _, err := h.Svc.SendMessage(r.Context(), tid, u.UserID, in.Body); err != nil {
		h.Log.Warn("pm send", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	if err := h.patchThreadMessages(r.Context(), sse, tid, u.UserID); err != nil {
		h.Log.Warn("pm patch", "err", err)
	}
	_ = sse.PatchSignals([]byte(`{"pm_send_body":""}`))
}

func (h *Handler) PostAccept(w http.ResponseWriter, r *http.Request) {
	h.statusChange(w, r, true)
}
func (h *Handler) PostDecline(w http.ResponseWriter, r *http.Request) {
	h.statusChange(w, r, false)
}
func (h *Handler) statusChange(w http.ResponseWriter, r *http.Request, accept bool) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	tid := chi.URLParam(r, "id")
	var err error
	if accept {
		err = h.Svc.Accept(r.Context(), tid, u.UserID)
	} else {
		err = h.Svc.Decline(r.Context(), tid, u.UserID)
	}
	if err != nil {
		h.Log.Warn("pm status", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/messages/" + tid)
}

func (h *Handler) PostRead(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	tid := chi.URLParam(r, "id")
	if err := h.Svc.MarkRead(r.Context(), tid, u.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetBadge renders the badge fragment (initial sync — SSE pushes deltas).
func (h *Handler) GetBadge(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	count, err := h.badgeCount(r.Context(), u.UserID)
	if err != nil {
		h.Log.Warn("pm badge", "err", err)
		count = 0
	}
	_ = webtempl.MessagesBadge(count).Render(r.Context(), w)
}

func (h *Handler) badgeCount(ctx context.Context, userID string) (int, error) {
	pending, err := h.Repo.PendingCountForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	unread, err := h.Repo.UnreadCountForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	return pending + unread, nil
}

// GetStream is a long-lived SSE the layout opens once per page. Pushes the
// updated badge whenever this user's per-user bus fires.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sse := datastar.NewSSE(w, r)

	if n, err := h.badgeCount(r.Context(), u.UserID); err == nil {
		_ = sse.PatchElementTempl(webtempl.MessagesBadge(n), datastar.WithModeOuter())
	}

	local, unsub := h.Bus.Subscribe(u.UserID)
	defer unsub()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// keep-alive comment via empty patch
			_ = sse.PatchSignals([]byte(`{}`))
		case <-local:
			n, err := h.badgeCount(r.Context(), u.UserID)
			if err != nil {
				continue
			}
			_ = sse.PatchElementTempl(webtempl.MessagesBadge(n), datastar.WithModeOuter())
		}
	}
}

// GetThreadStream pushes message list updates for one thread.
func (h *Handler) GetThreadStream(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewer(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	tid := chi.URLParam(r, "id")
	t, err := h.Repo.ThreadByID(r.Context(), tid)
	if err != nil || !t.HasMember(u.UserID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sse := datastar.NewSSE(w, r)

	if err := h.patchThreadMessages(r.Context(), sse, tid, u.UserID); err != nil {
		h.Log.Warn("pm thread patch", "err", err)
	}

	local, unsub := h.Bus.Subscribe(u.UserID)
	defer unsub()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
			if err := h.patchThreadMessages(r.Context(), sse, tid, u.UserID); err != nil {
				h.Log.Warn("pm thread patch", "err", err)
			}
			_ = h.Svc.MarkRead(r.Context(), tid, u.UserID)
		}
	}
}

func (h *Handler) patchThreadMessages(ctx context.Context, sse *datastar.ServerSentEventGenerator, threadID, viewerID string) error {
	msgs, err := h.Repo.MessagesByThread(ctx, threadID)
	if err != nil {
		return err
	}
	views := toMsgViews(msgs, viewerID)
	return sse.PatchElementTempl(webtempl.MessagesThreadList(views), datastar.WithModeOuter())
}

func toMsgViews(msgs []Message, viewerID string) []webtempl.MessagesMsgView {
	out := make([]webtempl.MessagesMsgView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, webtempl.MessagesMsgView{
			ID:        m.ID,
			BodyHTML:  m.BodyHTML,
			CreatedAt: m.CreatedAt,
			IsMine:    m.AuthorUserID == viewerID,
		})
	}
	return out
}

// resolveName picks the user's display name from their most recent
// membership, falling back to email when they aren't yet in any community.
func (h *Handler) resolveName(ctx context.Context, userID string) string {
	if n, err := h.Repo.DisplayName(ctx, userID); err == nil && n != "" {
		return n
	}
	if u, err := h.AuthRepo.UserByID(ctx, userID); err == nil {
		return u.Email
	}
	return userID
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

var _ = errors.New
