package forum

import (
	"context"
	"encoding/json"
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

	"github.com/atvirokodosprendimai/forumchat/internal/agentlimit"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc      *Service
	Repo     *Repo
	Chat     *chat.Service
	ChatRepo *chat.Repo
	ChatBus  *chat.Bus
	// ChatNewMsgBus is the strict "new chat message" fan-out — wakes
	// the cross-page chat-events SSE so a viewer on /forum still
	// hears a ping when a thread-announce row hits chat. Optional;
	// nil-safe.
	ChatNewMsgBus *chat.Bus
	Bus           *Bus
	NATS          *nats.Conn
	Uploads       *uploads.Store
	// PushNotify dispatches a web-push notification. Optional. Wired
	// in main.go to the push package's Sender so this package doesn't
	// import push.
	PushNotify func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string)
	// RelayOut, if non-nil, mirrors the chat thread-announce (a new forum
	// thread surfaced in #general) to outbound webhooks so external chat
	// mirrors hear about new threads. Wired in main.go to the same
	// webhooks.Relay.Dispatch as chat. nil disables the relay (no-op).
	RelayOut func(communityID, channelID, authorName, bodyMD, channelName string, attachmentUploadIDs []string)
	// RelayThread, if non-nil, mirrors forum-thread content to outbound webhooks
	// with the thread identity attached (so a bridge can group messages into one
	// external thread, e.g. a Matrix m.thread). root=true marks the opening
	// message; postID is "" for the opener. Only human-authored content reaches
	// here — inbound-webhook posts are bot posts and bypass this path (no echo).
	// Wired in main.go to webhooks.Relay.DispatchForum; nil disables it.
	RelayThread func(communityID, channelID, channelName, author, bodyMD, threadID, postID, subject string, root bool)
	// OnAgentReply, when set, is fired after a human reply lands in an
	// agent-owned thread (thread.AgentID set) — the agent re-runs over the full
	// thread history and streams the next post. Wired in main.go to chatagents;
	// nil when AI is disabled. The thread's agent_id plus the replier's id are
	// passed (the latter for rate limiting); forum stays agent-free. Runs
	// synchronously so a throttled reply can surface a notice; generation
	// detaches inside the runner. A bot post never fires it (loop guard).
	OnAgentReply func(ctx context.Context, communityID, threadID, agentID, userID string, isSuperAdmin bool) AgentReplyResult
	// Flood rate-limits thread + reply creation per member (FIX1 H7). Each one
	// fans out to SSE + NATS + push + outbound webhooks, so it needs a cap.
	// Nil = unlimited. Shared with the other send surfaces.
	Flood         *agentlimit.Limiter
	CommunityID   string
	CommunityName string
	BaseURL       string
	Log           *slog.Logger
}

// ForumPostPerMinute caps new threads + replies per member per minute (FIX1 H7).
const ForumPostPerMinute = 30

// floodReject applies the per-user forum flood budget. On denial it opens an SSE,
// patches a retry message into the given error-fragment slot, and returns true
// so the caller returns. Super-admins and a nil limiter never trip it. The SSE
// is opened only on denial, so a caller creating its own SSE on the allow path
// won't double-flush.
func (h *Handler) floodReject(w http.ResponseWriter, r *http.Request, id auth.Identity, slot string) bool {
	if id.IsSuperAdmin || h.Flood == nil {
		return false
	}
	ok, retry := h.Flood.AllowRecord("forumpost:"+h.cid(r.Context())+":"+id.User.ID, ForumPostPerMinute)
	if ok {
		return false
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.ErrorFragment(slot, fmt.Sprintf("You're posting too fast — try again in %ds.", int(retry.Seconds())+1)))
	return true
}

// AgentReplyResult reports whether an agent-thread reply was rate-limited, so
// PostReply can surface a notice. The zero value means "not rate-limited".
type AgentReplyResult struct {
	RateLimited bool
	RetryAfter  time.Duration
}

const PasteImageMaxBytes = 1 << 20

// relayThreadAnnounce mirrors a new-thread announcement to outbound webhooks.
// The announce row lands in #general; resolve it so the relay matches webhooks
// bound to that channel. A clean text body (not the announce HTML) keeps
// Slack/Discord output readable. When RelayThread is wired (webhooks on) the
// announce carries the thread identity (root message); otherwise it falls back
// to the plain RelayOut. No-op when no relay or chat repo is present.
func (h *Handler) relayThreadAnnounce(ctx context.Context, communityID, authorName, threadID, subject, link string) {
	if h.ChatRepo == nil || (h.RelayOut == nil && h.RelayThread == nil) {
		return
	}
	ch, err := h.ChatRepo.DefaultChannel(ctx, communityID)
	if err != nil {
		h.Log.Warn("relay thread-announce: default channel", "err", err)
		return
	}
	body := "📋 started a thread: " + subject + "\n" + link
	if h.RelayThread != nil {
		h.RelayThread(communityID, ch.ID, ch.Name, authorName, body, threadID, "", subject, true)
		return
	}
	h.RelayOut(communityID, ch.ID, authorName, body, ch.Name, nil)
}

// relayForumReply mirrors a human forum reply to outbound webhooks, tagged with
// the thread identity so a bridge posts it under the matching external thread.
// Rides the default channel like the announce. No-op without RelayThread.
func (h *Handler) relayForumReply(ctx context.Context, t Thread, author, body, postID string) {
	if h.RelayThread == nil || h.ChatRepo == nil {
		return
	}
	ch, err := h.ChatRepo.DefaultChannel(ctx, t.CommunityID)
	if err != nil {
		h.Log.Warn("relay forum reply: default channel", "err", err)
		return
	}
	h.RelayThread(t.CommunityID, ch.ID, ch.Name, author, body, t.ID, postID, t.Subject, false)
}

func (h *Handler) cid(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.ID
	}
	return h.CommunityID
}

func (h *Handler) cname(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.Name
	}
	return h.CommunityName
}

func (h *Handler) cslug(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.Slug
	}
	return ""
}

// threadInCommunity loads a thread by id and confirms it belongs to the
// URL-slug community. The forum routes rebind role to the slug community,
// but resolved threads by raw global id with no tenant check — so a
// mod/admin of community A could read/delete/rename/hard-delete community
// B's threads. Writes 404 and returns ok=false on miss or cross-tenant
// mismatch. Call it BEFORE opening an SSE response (it writes headers).
func (h *Handler) threadInCommunity(w http.ResponseWriter, r *http.Request, threadID string) (Thread, bool) {
	t, err := h.Repo.GetThread(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return Thread{}, false
	}
	if t.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return Thread{}, false
	}
	return t, true
}

// attachPastedImage prepends a markdown image link to body if an image data
// URL was pasted. Returns the new body; image errors are logged and ignored
// so the textual content still posts.
func (h *Handler) attachPastedImage(r *http.Request, userID, body, imageData string) string {
	if imageData == "" || h.Uploads == nil {
		return body
	}
	u, err := h.Uploads.SaveDataURL(r.Context(), userID, h.cid(r.Context()), imageData, PasteImageMaxBytes)
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

func (h *Handler) broadcastThread(ctx context.Context, threadID string) {
	h.BroadcastThreadID(h.cid(ctx), threadID)
}

// BroadcastThreadID wakes open viewers of a thread (in-process Bus + NATS) so
// they refetch and re-render. Exported for cross-package callers (e.g. the
// webhooks inbound bridge) that hold the community id but no request context.
func (h *Handler) BroadcastThreadID(communityID, threadID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(threadID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ForumThreadSubject(communityID, threadID), []byte("changed"))
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
			AuthorAvatar: p.AuthorAvatar,
			QuotedAuthor: p.QuotedAuthor,
			QuotedBody:   p.QuotedBody,
			BodyHTML:     p.BodyHTML,
			BodyMD:       p.BodyMarkdown,
			CreatedAt:    p.CreatedAt,
			Deleted:      p.IsDeleted(),
			// Bot posts are deletable by mods only (no per-author edit grace).
			CanEdit:      (!p.IsBot() && p.AuthorID == currentUserID && now.Sub(p.CreatedAt) <= h.Svc.EditGrace) || isMod,
			TitleSnippet: render.AutoTitle(p.BodyMarkdown),
			IsBot:        p.IsBot(),
			GenStatus:    p.GenStatus,
			ToolCalls:    decodeToolChips(p.ToolCalls),
		})
	}
	return pv, nil
}

// decodeToolChips parses an agent reply post's JSON tool trace into the chip
// view. The JSON (agent.EncodeToolCalls) field names match AgentToolView
// case-insensitively, so forum needn't import agent. Empty / bad → nil.
func decodeToolChips(s string) []webtempl.AgentToolView {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []webtempl.AgentToolView
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

const ThreadLimit = 50

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.cname(r.Context()), CommunitySlug: h.cslug(r.Context())}
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
	ts, err := h.Repo.ListThreadsFiltered(r.Context(), h.cid(r.Context()), repoStatus, q, ThreadLimit)
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
	if h.floodReject(w, r, id, "thread-error") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in newThreadSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	subject := strings.TrimSpace(in.Subject)
	body := strings.TrimSpace(in.Body)
	body = h.attachPastedImage(r, id.User.ID, body, in.ImageData)
	if subject == "" || body == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("thread-error", "Subject and body required"))
		return
	}
	t, err := h.Svc.CreateThread(r.Context(), CreateThreadInput{
		CommunityID:  h.cid(r.Context()),
		AuthorID:     id.User.ID,
		Subject:      subject,
		BodyMarkdown: body,
	})
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("thread-error", err.Error()))
		return
	}

	if h.Chat != nil {
		link := fmt.Sprintf(`%s/c/%s/forum/%s`, strings.TrimRight(h.BaseURL, "/"), h.cslug(r.Context()), t.ID)
		threadID := t.ID
		announceHTML := buildThreadAnnounce(id.Membership.ShownName(), link, t.Subject, t.BodyMarkdown)
		_, err := h.Chat.PostSystem(r.Context(), h.cid(r.Context()), announceHTML, chat.KindThreadAnnounce, &threadID)
		if err != nil {
			h.Log.Error("post thread-announce", "err", err)
		} else {
			h.relayThreadAnnounce(r.Context(), h.cid(r.Context()), id.Membership.ShownName(), t.ID, t.Subject, link)
			if h.ChatNewMsgBus != nil {
				h.ChatNewMsgBus.Broadcast("")
			}
			if h.NATS != nil && h.NATS.IsConnected() {
				// Two fan-outs: chat-page subscribers re-render via the
				// generic chat subject; cross-page event listeners ping
				// off the strict chat.new subject.
				_ = h.NATS.Publish(natsx.ChatSubject(h.cid(r.Context())), []byte("changed"))
				_ = h.NATS.Publish(natsx.ChatNewSubject(h.cid(r.Context())), []byte("new"))
			}
		}
	}

	// Background push: broadcast the new thread to every community
	// subscriber opted in to thread_new. Runs detached so the redirect
	// isn't blocked by the push services.
	if h.PushNotify != nil {
		cid := h.cid(r.Context())
		cslug := h.cslug(r.Context())
		authorName := id.Membership.ShownName()
		threadURL := "/c/" + cslug + "/forum/" + t.ID
		subjectCopy := t.Subject
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h.PushNotify(ctx, cid, "thread_new", nil,
				"New thread: "+subjectCopy,
				authorName+" started a new forum thread.",
				threadURL)
		}()
	}

	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + t.ID)
}

func (h *Handler) GetThread(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	threadID := chi.URLParam(r, "id")
	t, ok := h.threadInCommunity(w, r, threadID)
	if !ok {
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
	if _, ok := h.threadInCommunity(w, r, threadID); !ok {
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := render.NewSSE(w, r)

	// Subscribe BEFORE the initial render so a broadcast landing between first
	// paint and subscription isn't lost — the buffered channel holds it.
	// Mirrors the agent pane (internal/agent/handler.go GetStream).
	local, unsubscribe := h.Bus.Subscribe(threadID)
	defer unsubscribe()

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.ForumThreadSubject(h.cid(r.Context()), threadID), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe forum thread", "err", err)
			natsCh = nil
		}
	}

	var generating bool
	push := func() error {
		pv, err := h.loadPostViews(r.Context(), threadID, id.User.ID, isMod)
		if err != nil {
			return nil
		}
		generating = anyGenerating(pv)
		if err := sse.PatchElementTempl(
			webtempl.ThreadPosts(h.cslug(r.Context()), threadID, pv),
			datastar.WithModeOuter(),
		); err != nil {
			return err
		}
		return sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	}
	if err := push(); err != nil {
		return
	}

	// Belt-and-braces re-sync: while an agent reply is generating, re-render
	// every interval even if a 100ms streaming broadcast was coalesced into the
	// buffered channel or missed. Without this, a connection opened BEFORE the
	// reply only catches the terminal broadcast — the placeholder + token stream
	// never paint (a fresh connection works because its initial push catches the
	// live state). Event-driven stays the fast path; this is the fallback.
	resync := time.NewTicker(400 * time.Millisecond)
	defer resync.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
		case _, alive := <-natsCh:
			if !alive {
				natsCh = nil
				continue
			}
		case <-resync.C:
			if !generating {
				continue // idle thread — don't re-render for nothing
			}
		}
		if err := push(); err != nil {
			return
		}
	}
}

// anyGenerating reports whether any post in the thread is a bot reply still
// streaming (gen_status="generating"), so the stream keeps re-syncing until the
// answer is complete.
func anyGenerating(pv []webtempl.PostView) bool {
	for _, p := range pv {
		if p.GenStatus == GenGenerating {
			return true
		}
	}
	return false
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
	t, ok := h.threadInCommunity(w, r, threadID)
	if !ok {
		return
	}
	if h.floodReject(w, r, id, "reply-error") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in replySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
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
	post, err := h.Svc.CreatePost(r.Context(), CreatePostInput{
		ThreadID: threadID, AuthorID: id.User.ID,
		QuotedPostID: quoted, BodyMarkdown: body,
	})
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("reply-error", err.Error()))
		return
	}
	// Patch posts immediately for this client; broadcast for everyone else.
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	if pv, err := h.loadPostViews(r.Context(), threadID, id.User.ID, isMod); err == nil {
		_ = sse.PatchElementTempl(webtempl.ThreadPosts(h.cslug(r.Context()), threadID, pv), datastar.WithModeOuter())
	}
	_ = sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	_ = sse.PatchSignals([]byte(`{"body":"","quoted_post_id":"","_reply_quote_label":"","image_data":""}`))
	h.broadcastThread(r.Context(), threadID)

	// One thread lookup serves two post-reply paths:
	//   - agent threads (AgentID set): reply-as-prompt re-runs the agent over the
	//     full history; it answers as the next post. Synchronous so a throttled
	//     reply can surface a notice; the model generation detaches inside the
	//     runner. Bot posts are inserted via InsertBotPost, never through
	//     PostReply, so they can't re-trigger.
	//   - plain threads: mirror the human reply to outbound webhooks tagged with
	//     the thread identity (Matrix-thread sync). Agent threads stay local.
	if h.OnAgentReply != nil || h.RelayThread != nil {
		switch {
		case t.AgentID != nil && h.OnAgentReply != nil:
			res := h.OnAgentReply(r.Context(), h.cid(r.Context()), threadID, *t.AgentID, id.User.ID, id.IsSuperAdmin)
			if res.RateLimited {
				_ = sse.PatchElementTempl(webtempl.AgentRateLimitNotice("thread-agent-notice", res.RetryAfter))
			}
		case t.AgentID == nil:
			h.relayForumReply(r.Context(), t, id.Membership.ShownName(), body, post.ID)
		}
	}
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
	t, ok := h.threadInCommunity(w, r, threadID)
	if !ok {
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
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + threadID)
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
	if _, ok := h.threadInCommunity(w, r, threadID); !ok {
		return
	}
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
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + threadID)
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
	if _, ok := h.threadInCommunity(w, r, threadID); !ok {
		return
	}
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
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum")
}

func (h *Handler) PostDeleteThread(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threadID := chi.URLParam(r, "id")
	t, ok := h.threadInCommunity(w, r, threadID)
	if !ok {
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
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum")
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
	if msg.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	authorMatch := msg.AuthorID != nil && *msg.AuthorID == id.User.ID
	if !authorMatch && !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if msg.PromotedThreadID != nil {
		sse := render.NewSSE(w, r)
		_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + *msg.PromotedThreadID)
		return
	}

	// Carry the reply chain: when the promoted message is a reply (B → A), the
	// PARENT (A) becomes the thread root and the clicked reply (B) falls into the
	// thread as its first post. Promoting a non-reply keeps the single-message
	// behaviour. A deleted parent is ignored (clicked message stays the root).
	root := msg
	var replyChild *chat.Message
	if msg.ReplyToID != nil {
		if parent, perr := h.ChatRepo.ByID(r.Context(), *msg.ReplyToID); perr == nil && parent.DeletedAt == nil {
			// If the parent already opened a thread, just fold this reply into it
			// rather than spawning a duplicate.
			if parent.PromotedThreadID != nil {
				h.foldReplyIntoThread(w, r, id, msg, *parent.PromotedThreadID)
				return
			}
			child := msg
			root, replyChild = parent, &child
		}
	}

	subject := deriveSubject(root.BodyMarkdown)
	if subject == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	threadAuthorID := id.User.ID
	if root.AuthorID != nil {
		threadAuthorID = *root.AuthorID
	}
	t, err := h.Svc.CreateThread(r.Context(), CreateThreadInput{
		CommunityID:  h.cid(r.Context()),
		AuthorID:     threadAuthorID,
		Subject:      subject,
		BodyMarkdown: root.BodyMarkdown,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Claim the root chat message for this thread atomically. If somebody else
	// won the race, drop our thread and redirect to theirs.
	claimed, err := h.ChatRepo.MarkPromoted(r.Context(), root.ID, t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !claimed {
		_, _ = h.Repo.HardDeleteThread(r.Context(), t.ID)
		fresh, err2 := h.ChatRepo.ByID(r.Context(), root.ID)
		sse := render.NewSSE(w, r)
		if err2 == nil && fresh.PromotedThreadID != nil {
			_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + *fresh.PromotedThreadID)
		} else {
			http.Error(w, "promote race", http.StatusConflict)
		}
		return
	}
	// Add the clicked reply as the thread's first post, attributed to its author
	// (falling back to the promoter for a bot/webhook message), then link it to
	// the thread so its own "→ thread" button redirects here.
	var firstPost *Post
	if replyChild != nil {
		childAuthorID := id.User.ID
		if replyChild.AuthorID != nil {
			childAuthorID = *replyChild.AuthorID
		}
		if p, perr := h.Svc.CreatePost(r.Context(), CreatePostInput{
			ThreadID:     t.ID,
			AuthorID:     childAuthorID,
			BodyMarkdown: replyChild.BodyMarkdown,
		}); perr != nil {
			h.Log.Warn("promote: add reply post", "err", perr)
		} else {
			firstPost = &p
			_, _ = h.ChatRepo.MarkPromoted(r.Context(), replyChild.ID, t.ID)
		}
	}
	announceName := root.AuthorName
	if announceName == "" {
		announceName = id.Membership.ShownName()
	}
	h.announcePromotedThread(r.Context(), h.cid(r.Context()), h.cslug(r.Context()), announceName, t.ID, t.Subject, root.BodyMarkdown)
	// Mirror the promoted reply (the thread's first post) outbound too, tagged
	// with the thread identity. Without this the webhook side would carry the
	// thread-announce (root) plus later replies but silently drop the first
	// post's content — the thread would mirror with one message missing.
	if firstPost != nil && replyChild != nil {
		replyName := replyChild.AuthorName
		if replyName == "" {
			replyName = id.Membership.ShownName()
		}
		h.relayForumReply(r.Context(), t, replyName, replyChild.BodyMarkdown, firstPost.ID)
	}
	h.broadcastChatStructural(h.cid(r.Context()))
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + t.ID)
}

// announcePromotedThread posts the "→ thread" announce row into chat and mirrors
// it to outbound webhooks. Extracted from PostPromoteChat so the connector
// promote bridge (PromoteChatMessageByID) reuses the exact same announce + relay
// instead of duplicating it. announceName is the display name shown in the
// announce (the message author, or a fallback the caller supplies).
func (h *Handler) announcePromotedThread(ctx context.Context, communityID, slug, announceName, threadID, subject, rootBodyMD string) {
	if h.Chat == nil {
		return
	}
	link := fmt.Sprintf(`%s/c/%s/forum/%s`, strings.TrimRight(h.BaseURL, "/"), slug, threadID)
	tID := threadID
	announceHTML := buildThreadAnnounce(announceName, link, subject, rootBodyMD)
	if _, err := h.Chat.PostSystem(ctx, communityID, announceHTML, chat.KindThreadAnnounce, &tID); err != nil {
		h.Log.Error("promote thread-announce", "err", err)
		return
	}
	h.relayThreadAnnounce(ctx, communityID, announceName, threadID, subject, link)
}

// broadcastChatStructural refreshes open chat tabs + cross-page listeners after a
// structural chat change (a promote drops a thread_announce row). Extracted from
// PostPromoteChat so the connector promote bridge fans out identically.
func (h *Handler) broadcastChatStructural(communityID string) {
	if h.ChatBus != nil {
		h.ChatBus.Broadcast("")
	}
	if h.ChatNewMsgBus != nil {
		h.ChatNewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte("changed"))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
}

// PromoteChatMessageByID promotes a single chat message into a forum thread and
// returns the new thread id. It is the reusable, identity-free core behind the
// click-path PostPromoteChat, exposed for the connectors CapPromote seam (wired
// in main.go so neither package imports the other). byUserID authors the thread
// (the connector's member); slug is needed for the announce link. Unlike the
// click path it does NOT fold a reply chain — a programmatic promote takes the
// message as-is. It is race-safe (MarkPromoted) and idempotent (an
// already-promoted message returns its existing thread id).
func (h *Handler) PromoteChatMessageByID(ctx context.Context, communityID, slug, byUserID, msgID string) (string, error) {
	if h.ChatRepo == nil {
		return "", fmt.Errorf("promotion not wired")
	}
	msg, err := h.ChatRepo.ByID(ctx, msgID) // live, non-deleted only
	if err != nil {
		return "", err
	}
	if msg.CommunityID != communityID {
		return "", fmt.Errorf("message not in this community")
	}
	if msg.PromotedThreadID != nil {
		return *msg.PromotedThreadID, nil // idempotent
	}
	subject := deriveSubject(msg.BodyMarkdown)
	if subject == "" {
		return "", fmt.Errorf("empty message")
	}
	// Author the thread as the message's own author when present (so it reads as
	// theirs), else the promoting connector member.
	authorID := byUserID
	if msg.AuthorID != nil {
		authorID = *msg.AuthorID
	}
	t, err := h.Svc.CreateThread(ctx, CreateThreadInput{
		CommunityID:  communityID,
		AuthorID:     authorID,
		Subject:      subject,
		BodyMarkdown: msg.BodyMarkdown,
	})
	if err != nil {
		return "", err
	}
	// Claim the chat message for this thread atomically; if another caller won the
	// race, drop our thread and return theirs.
	claimed, err := h.ChatRepo.MarkPromoted(ctx, msg.ID, t.ID)
	if err != nil {
		return "", err
	}
	if !claimed {
		_, _ = h.Repo.HardDeleteThread(ctx, t.ID)
		if fresh, ferr := h.ChatRepo.ByID(ctx, msg.ID); ferr == nil && fresh.PromotedThreadID != nil {
			return *fresh.PromotedThreadID, nil
		}
		return "", fmt.Errorf("promote race")
	}
	announceName := msg.AuthorName
	if announceName == "" {
		announceName = "a member"
	}
	h.announcePromotedThread(ctx, communityID, slug, announceName, t.ID, t.Subject, msg.BodyMarkdown)
	h.broadcastChatStructural(communityID)
	return t.ID, nil
}

// foldReplyIntoThread appends a promoted reply to the thread its parent already
// opened (instead of spawning a duplicate), attributing the post to the reply's
// author — falling back to the promoter for a bot/webhook message — links the
// reply to that thread, wakes open thread viewers, and redirects there.
func (h *Handler) foldReplyIntoThread(w http.ResponseWriter, r *http.Request, id auth.Identity, msg chat.Message, threadID string) {
	authorID := id.User.ID
	if msg.AuthorID != nil {
		authorID = *msg.AuthorID
	}
	post, err := h.Svc.CreatePost(r.Context(), CreatePostInput{
		ThreadID:     threadID,
		AuthorID:     authorID,
		BodyMarkdown: msg.BodyMarkdown,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = h.ChatRepo.MarkPromoted(r.Context(), msg.ID, threadID)
	// Mirror the folded reply outbound with the thread identity, exactly like a
	// normal forum reply — otherwise the webhook side loses this message.
	if th, gerr := h.Repo.GetThread(r.Context(), threadID); gerr == nil {
		replyName := msg.AuthorName
		if replyName == "" {
			replyName = id.Membership.ShownName()
		}
		h.relayForumReply(r.Context(), th, replyName, msg.BodyMarkdown, post.ID)
	}
	h.BroadcastThreadID(h.cid(r.Context()), threadID)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/forum/" + threadID)
}

// CreateAgentThread opens an agent-owned forum thread for a triggered
// chat-agent and announces it back in chat. Returns the new thread id. This is
// the chat→forum bridge for chat-agents: wired into chatagents in main.go so
// neither package imports the other. communityID/slug are passed explicitly
// because the caller runs detached (no community in ctx). The thread is
// authored by the triggering human (authorID); agentID marks it for replies.
func (h *Handler) CreateAgentThread(ctx context.Context, communityID, slug, authorID, agentID, agentName, prompt string) (string, error) {
	subject := deriveSubject(prompt)
	if subject == "" {
		subject = agentName + " conversation"
	}
	aid := agentID
	t, err := h.Svc.CreateThread(ctx, CreateThreadInput{
		CommunityID:  communityID,
		AuthorID:     authorID,
		Subject:      subject,
		BodyMarkdown: prompt,
		AgentID:      &aid,
	})
	if err != nil {
		return "", err
	}
	if h.Chat != nil {
		link := fmt.Sprintf("%s/c/%s/forum/%s", strings.TrimRight(h.BaseURL, "/"), slug, t.ID)
		threadID := t.ID
		announceHTML := buildThreadAnnounce(agentName, link, t.Subject, prompt)
		if _, err := h.Chat.PostSystem(ctx, communityID, announceHTML, chat.KindThreadAnnounce, &threadID); err != nil {
			h.Log.Error("agent thread-announce", "err", err)
		} else {
			h.relayThreadAnnounce(ctx, communityID, agentName, t.ID, t.Subject, link)
		}
	}
	if h.ChatBus != nil {
		h.ChatBus.Broadcast("")
	}
	if h.ChatNewMsgBus != nil {
		h.ChatNewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte("changed"))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
	return t.ID, nil
}

// OpenWebhookThread opens a forum thread for an inbound webhook carrying a
// thread_key AND announces it back in chat — the piece the raw
// Svc.CreateWebhookThread call was missing, so a Matrix-thread mirror created a
// forum thread silently with no #general bubble. Modelled on CreateAgentThread
// (the other detached chat→forum bridge): create via the service, post the
// thread_announce system message, then wake open chat tabs (Bus + NATS).
//
// It deliberately does NOT relayThreadAnnounce: the thread was opened FROM an
// inbound bridge, so echoing the announce back out would loop it to the
// originating Matrix room. communityID/slug are explicit because the inbound
// /hooks route runs with no community in context.
func (h *Handler) OpenWebhookThread(ctx context.Context, communityID, slug, author, subject, markdown string) (string, error) {
	t, err := h.Svc.CreateWebhookThread(ctx, communityID, author, subject, markdown)
	if err != nil {
		return "", err
	}
	if h.Chat != nil {
		link := fmt.Sprintf("%s/c/%s/forum/%s", strings.TrimRight(h.BaseURL, "/"), slug, t.ID)
		threadID := t.ID
		announceHTML := buildThreadAnnounce(author, link, t.Subject, markdown)
		if _, err := h.Chat.PostSystem(ctx, communityID, announceHTML, chat.KindThreadAnnounce, &threadID); err != nil {
			h.Log.Error("webhook thread-announce", "err", err)
		}
	}
	if h.ChatBus != nil {
		h.ChatBus.Broadcast("")
	}
	if h.ChatNewMsgBus != nil {
		h.ChatNewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte("changed"))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
	return t.ID, nil
}

// buildThreadAnnounce returns the chat fan-out HTML for a new thread. When
// the source body starts with an image (so the subject collapsed to
// "(image)"), we render a thumbnail link instead of the literal label.
func buildThreadAnnounce(authorName, link, subject, body string) string {
	if subject == "(image)" {
		if src := extractFirstImageURL(body); src != "" {
			return fmt.Sprintf(
				`<strong>%s</strong> started <a href="%s">thread</a> <a href="%s"><img class="thread-announce-img" src="%s" alt="thread image"></a>`,
				htmlEscape(authorName), htmlEscape(link), htmlEscape(link), htmlEscape(src),
			)
		}
	}
	return fmt.Sprintf(
		`<strong>%s</strong> started <a href="%s">thread</a>: <a href="%s">%s</a>`,
		htmlEscape(authorName), htmlEscape(link), htmlEscape(link), htmlEscape(subject),
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
	// Posts carry only a thread id; verify the parent thread is in this
	// community so a mod of another tenant can't delete posts here by id.
	if _, ok := h.threadInCommunity(w, r, p.ThreadID); !ok {
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
	sse := render.NewSSE(w, r)
	if pv, err := h.loadPostViews(r.Context(), p.ThreadID, id.User.ID, isMod); err == nil {
		_ = sse.PatchElementTempl(webtempl.ThreadPosts(h.cslug(r.Context()), p.ThreadID, pv), datastar.WithModeOuter())
	}
	_ = sse.PatchElementTempl(webtempl.ThreadScrollAnchor(), datastar.WithModeReplace())
	h.broadcastThread(r.Context(), p.ThreadID)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
