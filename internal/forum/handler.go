package forum

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc           *Service
	Repo          *Repo
	Chat          *chat.Service
	ChatRepo      *chat.Repo
	ChatBus       *chat.Bus
	Bus           *Bus
	NATS          *nats.Conn
	Uploads       *uploads.Store
	CommunityID   string
	CommunityName string
	BaseURL       string
	Log           *slog.Logger
}

const PasteImageMaxBytes = 1 << 20

// attachPastedImage prepends a markdown image link to body if an image data
// URL was pasted. Returns the new body; image errors are logged and ignored
// so the textual content still posts.
func (h *Handler) attachPastedImage(r *http.Request, userID, body, imageData string) string {
	if imageData == "" || h.Uploads == nil {
		return body
	}
	u, err := h.Uploads.SaveDataURL(r.Context(), userID, h.CommunityID, imageData, PasteImageMaxBytes)
	if err != nil {
		h.Log.Warn("paste image", "err", err)
		return body
	}
	url := h.Uploads.SignedURL(u.ID, userID, 24*time.Hour)
	img := "[![](" + url + ")](" + url + ")"
	if body == "" {
		return img
	}
	return img + "\n\n" + body
}

func (h *Handler) broadcastThread(threadID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(threadID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ForumThreadSubject(h.CommunityID, threadID), []byte("changed"))
	}
}

func (h *Handler) loadPostViews(ctx context.Context, threadID, currentUserID string, isMod bool) ([]webtempl.PostView, error) {
	posts, err := h.Repo.ListPosts(ctx, threadID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
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
			CanEdit:      (p.AuthorID == currentUserID && now.Sub(p.CreatedAt) <= h.Svc.EditGrace) || isMod,
		})
	}
	return pv, nil
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
	// Default view hides resolved threads. Explicit ?status=all shows every
	// thread; ?status=resolved shows only resolved.
	status := r.URL.Query().Get("status")
	switch status {
	case "resolved", "unresolved", "all":
		// keep as-is
	default:
		status = "unresolved"
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	repoStatus := status
	if repoStatus == "all" {
		repoStatus = ""
	}
	ts, err := h.Repo.ListThreadsFiltered(r.Context(), h.CommunityID, repoStatus, q, ThreadLimit)
	if err != nil {
		http.Error(w, "load threads: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]webtempl.ThreadRow, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, webtempl.ThreadRow{
			ID: t.ID, Subject: t.Subject, AuthorName: t.AuthorName,
			LastActivityAt: t.LastActivityAt, IsResolved: t.IsResolved(),
		})
	}
	_ = webtempl.ForumIndex(h.viewer(r), rows, status, q).Render(r.Context(), w)
}

func (h *Handler) GetNew(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.NewThreadPage(h.viewer(r)).Render(r.Context(), w)
}

type newThreadSignals struct {
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	ImageData string `json:"image_data"`
}

func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in newThreadSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	subject := strings.TrimSpace(in.Subject)
	body := strings.TrimSpace(in.Body)
	body = h.attachPastedImage(r, id.User.ID, body, in.ImageData)
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
		threadID := t.ID
		announceHTML := buildThreadAnnounce(id.Membership.DisplayName, link, t.Subject, t.BodyMarkdown)
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
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	now := time.Now()
	view := webtempl.ThreadView{
		ID: t.ID, Subject: t.Subject, AuthorName: t.AuthorName,
		BodyHTML: t.BodyHTML, CreatedAt: t.CreatedAt,
		CanEdit:    t.AuthorID == id.User.ID && now.Sub(t.CreatedAt) <= h.Svc.EditGrace,
		IsMod:      isMod,
		IsAdmin:    id.Membership.Role.AtLeast(auth.RoleAdmin),
		IsResolved: t.IsResolved(),
		CanResolve: t.AuthorID == id.User.ID || isMod,
	}
	pv, err := h.loadPostViews(r.Context(), t.ID, id.User.ID, isMod)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.ThreadPage(h.viewer(r), view, pv).Render(r.Context(), w)
}

// GetThreadStream is the per-thread SSE channel. On every local Bus signal or
// NATS ping, refetch posts and outer-morph #posts.
func (h *Handler) GetThreadStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threadID := chi.URLParam(r, "id")
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := datastar.NewSSE(w, r)

	push := func() error {
		pv, err := h.loadPostViews(r.Context(), threadID, id.User.ID, isMod)
		if err != nil {
			return nil
		}
		if err := sse.PatchElementTempl(
			webtempl.ThreadPosts(threadID, pv),
			datastar.WithModeOuter(),
		); err != nil {
			return err
		}
		return sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	}
	_ = push()

	local, unsubscribe := h.Bus.Subscribe(threadID)
	defer unsubscribe()

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.ForumThreadSubject(h.CommunityID, threadID), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe forum thread", "err", err)
			natsCh = nil
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
		case _, ok := <-natsCh:
			if !ok {
				natsCh = nil
				continue
			}
		}
		if err := push(); err != nil {
			return
		}
	}
}

type replySignals struct {
	Body         string `json:"body"`
	QuotedPostID string `json:"quoted_post_id"`
	ImageData    string `json:"image_data"`
}

func (h *Handler) PostReply(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threadID := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in replySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	body := strings.TrimSpace(in.Body)
	body = h.attachPastedImage(r, id.User.ID, body, in.ImageData)
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
	// Patch posts immediately for this client; broadcast for everyone else.
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	if pv, err := h.loadPostViews(r.Context(), threadID, id.User.ID, isMod); err == nil {
		_ = sse.PatchElementTempl(webtempl.ThreadPosts(threadID, pv), datastar.WithModeOuter())
	}
	_ = sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	_ = sse.PatchSignals([]byte(`{"body":"","quoted_post_id":"","reply_quote_label":"","image_data":""}`))
	h.broadcastThread(threadID)
}

// PostResolve / PostUnresolve toggle the resolved marker. Author of the
// thread or any moderator/admin may flip it.
func (h *Handler) postResolve(w http.ResponseWriter, r *http.Request, resolved bool) {
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
	if t.AuthorID != id.User.ID && !isMod {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if resolved {
		if err := h.Repo.MarkResolved(r.Context(), threadID, id.User.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if err := h.Repo.MarkUnresolved(r.Context(), threadID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum/" + threadID)
}

func (h *Handler) PostResolve(w http.ResponseWriter, r *http.Request)   { h.postResolve(w, r, true) }
func (h *Handler) PostUnresolve(w http.ResponseWriter, r *http.Request) { h.postResolve(w, r, false) }

type renameSignals struct {
	NewSubject string `json:"new_subject"`
}

// PostRename lets a moderator or admin rename a thread.
func (h *Handler) PostRename(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	threadID := chi.URLParam(r, "id")
	var in renameSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	subject := strings.TrimSpace(in.NewSubject)
	if subject == "" {
		http.Error(w, "subject required", http.StatusBadRequest)
		return
	}
	if len(subject) > 200 {
		subject = subject[:200]
	}
	if err := h.Repo.UpdateSubject(r.Context(), threadID, subject); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum/" + threadID)
}

// PostHardDeleteThread (admin) wipes the thread + posts + any uploads
// referenced in their bodies.
func (h *Handler) PostHardDeleteThread(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if !id.Membership.Role.AtLeast(auth.RoleAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	threadID := chi.URLParam(r, "id")
	uploadIDs, err := h.Repo.HardDeleteThread(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Uploads != nil {
		for _, uid := range uploadIDs {
			if err := h.Uploads.Delete(r.Context(), uid); err != nil {
				h.Log.Warn("hard-delete upload", "id", uid, "err", err)
			}
		}
	}
	if h.ChatRepo != nil {
		if err := h.ChatRepo.ClearPromoted(r.Context(), threadID); err != nil {
			h.Log.Warn("clear promoted_thread_id", "thread", threadID, "err", err)
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum")
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

// PostPromoteChat takes a chat message id and creates a forum thread whose
// subject + body come from that message. Author of the chat message OR
// mod/admin may promote. The original chat message stays put; the new
// thread fires the usual chat thread_announce via h.Chat.PostSystem.
func (h *Handler) PostPromoteChat(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	msgID := r.URL.Query().Get("id")
	if msgID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if h.ChatRepo == nil {
		http.Error(w, "promotion not wired", http.StatusInternalServerError)
		return
	}
	msg, err := h.ChatRepo.ByID(r.Context(), msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	authorMatch := msg.AuthorID != nil && *msg.AuthorID == id.User.ID
	if !authorMatch && !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if msg.PromotedThreadID != nil {
		sse := datastar.NewSSE(w, r)
		_ = sse.Redirect("/forum/" + *msg.PromotedThreadID)
		return
	}
	subject := deriveSubject(msg.BodyMarkdown)
	if subject == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	threadAuthorID := id.User.ID
	if msg.AuthorID != nil {
		threadAuthorID = *msg.AuthorID
	}
	t, err := h.Svc.CreateThread(r.Context(), CreateThreadInput{
		CommunityID:  h.CommunityID,
		AuthorID:     threadAuthorID,
		Subject:      subject,
		BodyMarkdown: msg.BodyMarkdown,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Claim the chat message for this thread atomically. If somebody else won
	// the race, drop our thread and redirect to theirs.
	claimed, err := h.ChatRepo.MarkPromoted(r.Context(), msg.ID, t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !claimed {
		_, _ = h.Repo.HardDeleteThread(r.Context(), t.ID)
		fresh, err2 := h.ChatRepo.ByID(r.Context(), msg.ID)
		sse := datastar.NewSSE(w, r)
		if err2 == nil && fresh.PromotedThreadID != nil {
			_ = sse.Redirect("/forum/" + *fresh.PromotedThreadID)
		} else {
			http.Error(w, "promote race", http.StatusConflict)
		}
		return
	}
	if h.Chat != nil {
		link := fmt.Sprintf(`%s/forum/%s`, strings.TrimRight(h.BaseURL, "/"), t.ID)
		announceName := msg.AuthorName
		if announceName == "" {
			announceName = id.Membership.DisplayName
		}
		threadID := t.ID
		announceHTML := buildThreadAnnounce(announceName, link, t.Subject, msg.BodyMarkdown)
		_, err := h.Chat.PostSystem(r.Context(), h.CommunityID, announceHTML, chat.KindThreadAnnounce, &threadID)
		if err != nil {
			h.Log.Error("promote thread-announce", "err", err)
		}
	}
	// Refresh open chat tabs so the thread_announce shows up live.
	if h.ChatBus != nil {
		h.ChatBus.Broadcast()
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte("changed"))
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/forum/" + t.ID)
}

// buildThreadAnnounce returns the chat fan-out HTML for a new thread. When
// the source body starts with an image (so the subject collapsed to
// "(image)"), we render a thumbnail link instead of the literal label.
func buildThreadAnnounce(authorName, link, subject, body string) string {
	if subject == "(image)" {
		if src := extractFirstImageURL(body); src != "" {
			return fmt.Sprintf(
				`<strong>%s</strong> started thread <a href="%s"><img class="thread-announce-img" src="%s" alt="thread image"></a>`,
				htmlEscape(authorName), htmlEscape(link), htmlEscape(src),
			)
		}
	}
	return fmt.Sprintf(
		`<strong>%s</strong> started thread: <a href="%s">%s</a>`,
		htmlEscape(authorName), htmlEscape(link), htmlEscape(subject),
	)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// imageMarkdownRE matches a leading markdown image (optionally wrapped in a
// link): `![alt](src)` or `[![alt](src)](href)`.
var imageMarkdownRE = regexp.MustCompile(`^\[?!\[[^\]]*\]\([^)]*\)\]?(?:\([^)]*\))?`)

// imageSrcRE captures the src URL of the leading markdown image (whether
// wrapped in a link or not). Used by the chat-promote announce so an
// image-only thread shows a thumbnail instead of the literal "(image)" label.
var imageSrcRE = regexp.MustCompile(`^\[?!\[[^\]]*\]\(([^)]+)\)`)

func extractFirstImageURL(body string) string {
	m := imageSrcRE.FindStringSubmatch(strings.TrimSpace(body))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// deriveSubject turns a chat-message body into a human-friendly thread
// subject. Strips leading markdown image syntax (so an image-only message
// promotes to "(image)" rather than the literal `![](/uploads/…)` link),
// otherwise uses the first line trimmed to 200 chars.
func deriveSubject(body string) string {
	line := strings.TrimSpace(firstLine(body))
	stripped := strings.TrimSpace(imageMarkdownRE.ReplaceAllString(line, ""))
	if stripped == "" && line != "" {
		return "(image)"
	}
	if len(stripped) > 200 {
		stripped = stripped[:200]
	}
	return stripped
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
	if pv, err := h.loadPostViews(r.Context(), p.ThreadID, id.User.ID, isMod); err == nil {
		_ = sse.PatchElementTempl(webtempl.ThreadPosts(p.ThreadID, pv), datastar.WithModeOuter())
	}
	_ = sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	h.broadcastThread(p.ThreadID)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
