package chat

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

const RecentLimit = 100

type Handler struct {
	Svc           *Service
	Repo          *Repo
	NATS          *nats.Conn
	Bus           *Bus
	// NewMsgBus carries ONLY "a new message just landed" events. The
	// global events SSE that powers cross-page notifications subscribes
	// here so it doesn't ping on edits, deletes, or read-receipt
	// updates (which still fire on Bus).
	NewMsgBus     *Bus
	Uploads       *uploads.Store
	// AuthRepo is used to resolve @mentions in chat messages to user ids
	// before firing PushNotify. Optional — when nil, mention notifications
	// are skipped silently.
	AuthRepo *auth.Repo
	// PushNotify dispatches a web-push notification. Wired in main.go to
	// the push package's Sender so this package doesn't import push.
	// userIDs may be empty to broadcast across the whole community.
	PushNotify    func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string)
	CommunityID   string
	CommunityName string
	Log           *slog.Logger

	// readBroadcastMu + readBroadcastAt throttle read-receipt fan-out so
	// every focus pulse from a user doesn't trigger a community-wide
	// fat-morph storm. Key is (community_id|user_id), value is the unix
	// time of the last broadcast.
	readBroadcastMu sync.Mutex
	readBroadcastAt map[string]time.Time
}

const PasteImageMaxBytes = 1 << 20 // 1 MiB

// cid / cname read the community resolved by the /c/{slug} middleware,
// falling back to the boot community embedded on the handler for
// transition / single-community deployments.
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

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.cname(r.Context()), CommunitySlug: h.cslug(r.Context())}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

func (h *Handler) loadRecent(ctx context.Context) ([]webtempl.MsgView, error) {
	msgs, err := h.Repo.Recent(ctx, h.cid(ctx), RecentLimit)
	if err != nil {
		return nil, err
	}
	return toMsgViews(msgs), nil
}

// loadRecentFor returns the latest N views and attaches the read-receipt
// list to the viewer's most recent own user message. Receipts are only
// computed for the viewer; other viewers see their own.
func (h *Handler) loadRecentFor(ctx context.Context, currentUserID string) ([]webtempl.MsgView, error) {
	views, err := h.loadRecent(ctx)
	if err != nil {
		return nil, err
	}
	h.attachReadReceipts(ctx, views, currentUserID)
	return views, nil
}

// attachReadReceipts walks views (desc, newest-first) and decorates the
// FIRST own non-deleted user message it finds with the readers whose
// last_read_at is at or past that message's created_at. No-op for guest
// viewers / empty list / missing repo.
func (h *Handler) attachReadReceipts(ctx context.Context, views []webtempl.MsgView, currentUserID string) {
	if currentUserID == "" || h.Repo == nil {
		return
	}
	for i := range views {
		v := views[i]
		if v.Kind != webtempl.MsgKindUser || v.Deleted || v.AuthorID != currentUserID {
			continue
		}
		readers, err := h.Repo.ReadersSince(ctx, h.cid(ctx), v.CreatedAt.Unix(), currentUserID, 30)
		if err != nil {
			h.Log.Warn("read receipts", "err", err)
			return
		}
		if len(readers) == 0 {
			return
		}
		out := make([]webtempl.ReaderView, 0, len(readers))
		for _, r := range readers {
			out = append(out, webtempl.ReaderView{
				UserID:      r.UserID,
				DisplayName: r.DisplayName,
				AvatarURL:   r.AvatarURL,
			})
		}
		views[i].ReadBy = out
		return
	}
}

// fatMorph emits the chat patches the UI expects:
//   1. #messages outer-morph → full latest-N list.
//   2. ExecuteScript → scroll #messages to its own bottom.
func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool, currentUserID, viewerName, slug string) error {
	if err := sse.PatchElementTempl(
		webtempl.MessagesContainer(views, isMod, currentUserID, viewerName, slug),
		datastar.WithModeOuter(),
	); err != nil {
		return err
	}
	return sse.ExecuteScript(
		`document.querySelector('#messages')?.scrollTo({top: 1e9, behavior: 'smooth'})`,
	)
}

// Welcome posts a one-shot "👋 Say hello to <name>" system message into
// the chat for the given community and broadcasts it. Best-effort: any
// error is logged and swallowed so callers don't have to roll back the
// caller's primary action (approve, join confirm, etc).
func (h *Handler) Welcome(ctx context.Context, communityID, displayName string) {
	name := strings.TrimSpace(displayName)
	if name == "" || communityID == "" || h.Svc == nil {
		return
	}
	body := "👋 Say hello to <strong>" + htmlEscape(name) + "</strong>!"
	if _, err := h.Svc.PostSystem(ctx, communityID, body, KindSystem, nil); err != nil {
		h.Log.Warn("welcome system msg", "err", err)
		return
	}
	if h.Bus != nil {
		h.Bus.Broadcast()
	}
	if h.NewMsgBus != nil {
		h.NewMsgBus.Broadcast()
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte("changed"))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
}

// htmlEscape is a tiny stand-in for html.EscapeString so chat doesn't
// have to pull in the whole `html` package for this single use.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// broadcast fans out a chat-changed signal locally (this process) AND over
// NATS (other processes). Either may be down; the other still works.
// Used for edits, deletes, read-receipt updates — anything where the
// chat page should re-render but nobody should hear a fresh ping.
func (h *Handler) broadcast(ctx context.Context) {
	if h.Bus != nil {
		h.Bus.Broadcast()
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.cid(ctx)), []byte("changed"))
	}
}

// broadcastNewMsg is broadcast() plus a fan-out on the strict
// "new-message-only" channel that the cross-page events stream
// listens on. Called from PostSend, Welcome, the forum bridge —
// anywhere a brand-new chat row appears. NOT from PostDelete or
// PostMarkRead.
func (h *Handler) broadcastNewMsg(ctx context.Context) {
	h.broadcast(ctx)
	if h.NewMsgBus != nil {
		h.NewMsgBus.Broadcast()
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatNewSubject(h.cid(ctx)), []byte("new"))
	}
}

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	views, err := h.loadRecentFor(r.Context(), id.User.ID)
	if err != nil {
		http.Error(w, "load chat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.ChatPage(webtempl.ChatPageData{
		Viewer:        h.viewer(r),
		IsMod:         id.Membership.Role.AtLeast(auth.RoleMod),
		CurrentUserID: id.User.ID,
		Messages:      views,
	}).Render(r.Context(), w)
}

type sendSignals struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id"`
	ImageData string `json:"image_data"`
}

func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	// 2 MB cap: 1 MB image after base64 (~1.33 MB on the wire) + text + JSON
	// framing. Defeats a runaway-paste turning into a memory grenade.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(in.Body)
	sse := render.NewSSE(w, r)

	if in.ImageData != "" && h.Uploads != nil {
		u, err := h.Uploads.SaveDataURL(r.Context(), id.User.ID, h.cid(r.Context()), in.ImageData, PasteImageMaxBytes)
		if err != nil {
			h.Log.Warn("paste image", "err", err)
		} else {
			url := h.Uploads.SignedURL(u.ID, id.User.ID, 24*time.Hour)
			imgMD := "[![](" + url + ")](" + url + ")"
			if body == "" {
				body = imgMD
			} else {
				body = imgMD + "\n\n" + body
			}
		}
	}

	if body == "" || len(body) > 4000 {
		return
	}
	var replyTo *string
	if rid := strings.TrimSpace(in.ReplyToID); rid != "" {
		replyTo = &rid
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:  h.cid(r.Context()),
		AuthorID:     id.User.ID,
		BodyMarkdown: body,
		ReplyToID:    replyTo,
	}); err != nil {
		h.Log.Error("send", "err", err)
		return
	}

	views, err := h.loadRecentFor(r.Context(), id.User.ID)
	if err == nil {
		_ = fatMorph(sse, views, id.Membership.Role.AtLeast(auth.RoleMod), id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()))
	}
	// Clear composer signals.
	_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":""}`))

	h.broadcastNewMsg(r.Context())

	// Fire-and-forget push notifications. Runs in the background so a
	// slow push service doesn't make the chat send look stalled to the
	// sender. Two distinct push kinds fire:
	//
	//   - "mention" — to every @name resolved out of the body.
	//   - "chat_new" — to every other approved member of the community
	//     EXCEPT the sender and those already targeted by a mention.
	//     The service worker suppresses the toast when a focused client
	//     is already viewing /chat, so this kind is safe to broadcast.
	if h.PushNotify != nil && h.AuthRepo != nil {
		cid := h.cid(r.Context())
		cslug := h.cslug(r.Context())
		senderID := id.User.ID
		senderName := id.Membership.DisplayName
		mentions := parseMentions(body)
		preview := bodyPreview(body, 120)
		url := "/c/" + cslug + "/chat"
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			mentioned := map[string]struct{}{}
			if len(mentions) > 0 {
				ids, err := h.AuthRepo.UserIDsByDisplayName(ctx, cid, mentions)
				if err == nil && len(ids) > 0 {
					ids = filterOut(ids, senderID)
					for _, uid := range ids {
						mentioned[uid] = struct{}{}
					}
					if len(ids) > 0 {
						title := senderName + " mentioned you"
						h.PushNotify(ctx, cid, "mention", ids, title, preview, url)
					}
				}
			}

			// chat_new — every other approved member not already pinged by
			// the mention loop. Skip when chat itself has no recipients.
			members, err := h.AuthRepo.ListMembers(ctx, cid)
			if err != nil || len(members) == 0 {
				return
			}
			rest := make([]string, 0, len(members))
			for _, m := range members {
				uid := m.Membership.UserID
				if uid == "" || uid == senderID {
					continue
				}
				if _, already := mentioned[uid]; already {
					continue
				}
				rest = append(rest, uid)
			}
			if len(rest) == 0 {
				return
			}
			title := senderName + " in " + h.cname(r.Context())
			h.PushNotify(ctx, cid, "chat_new", rest, title, preview, url)
		}()
	}
}

type markReadSignals struct {
	LastID string `json:"last_id"`
}

// PostMarkRead upserts the viewer's chat read high-water mark and, when
// the timestamp moved by enough, broadcasts a chat-changed signal so
// other tabs re-render the receipt stacks. Throttled to once per 2s per
// (user, community) so a typing/focus-pulse heavy client doesn't fan
// out a fat-morph storm.
func (h *Handler) PostMarkRead(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in markReadSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	cid := h.cid(r.Context())
	if err := h.Repo.MarkRead(r.Context(), id.User.ID, cid, strings.TrimSpace(in.LastID), time.Now()); err != nil {
		h.Log.Warn("mark read", "err", err)
		return
	}
	if !h.shouldBroadcastRead(cid, id.User.ID, time.Now()) {
		return
	}
	h.broadcast(r.Context())
}

// shouldBroadcastRead returns true at most once every 2s per (community,
// user). Read calls below the throttle still hit the DB (so the stack
// is correct on the next fat-morph) but don't kick a community-wide
// re-render.
func (h *Handler) shouldBroadcastRead(communityID, userID string, now time.Time) bool {
	const cooldown = 2 * time.Second
	key := communityID + "|" + userID
	h.readBroadcastMu.Lock()
	defer h.readBroadcastMu.Unlock()
	if h.readBroadcastAt == nil {
		h.readBroadcastAt = make(map[string]time.Time)
	}
	if last, ok := h.readBroadcastAt[key]; ok && now.Sub(last) < cooldown {
		return false
	}
	h.readBroadcastAt[key] = now
	return true
}

// parseMentions finds @name tokens in the body. Returns the unique
// lowercased name set (the membership query is case-insensitive).
func parseMentions(body string) []string {
	if body == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	var b strings.Builder
	in := false
	for _, r := range body {
		if r == '@' {
			in = true
			b.Reset()
			continue
		}
		if in {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				b.WriteRune(r)
				continue
			}
			// Token ended.
			if b.Len() >= 2 {
				k := strings.ToLower(b.String())
				if _, ok := seen[k]; !ok {
					seen[k] = struct{}{}
					out = append(out, k)
				}
			}
			in = false
		}
	}
	if in && b.Len() >= 2 {
		k := strings.ToLower(b.String())
		if _, ok := seen[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// bodyPreview returns the first N visible runes of the body with a
// trailing ellipsis when it was truncated.
func bodyPreview(body string, n int) string {
	body = strings.TrimSpace(body)
	count := 0
	for i := range body {
		if count >= n {
			return body[:i] + "…"
		}
		count++
	}
	return body
}

func filterOut(ids []string, drop string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := render.NewSSE(w, r)

	// Initial sync: on every (re)connection — including when the browser
	// re-establishes SSE after tab sleep — push the latest 100 immediately.
	// Without this, a reconnecting client would see stale messages until the
	// next chat event fires.
	if views, err := h.loadRecentFor(r.Context(), id.User.ID); err == nil {
		_ = fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()))
	}

	local, unsubscribe := h.Bus.Subscribe()
	defer unsubscribe()

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.ChatSubject(h.cid(r.Context())), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe", "err", err)
			natsCh = nil
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
			// fall through to refresh
		case _, ok := <-natsCh:
			if !ok {
				natsCh = nil
				continue
			}
		}
		views, err := h.loadRecentFor(r.Context(), id.User.ID)
		if err != nil {
			continue
		}
		if err := fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context())); err != nil {
			return
		}
	}
}

// GetEventsStream is the lightweight cross-page chat-event SSE.
// Mounted at /c/{slug}/chat/events and opened from layout.templ on
// every authed page in a community, it does NOTHING on the wire
// except emit `window.fcChatPing && window.fcChatPing()` whenever a
// genuinely new chat message lands. The client decides whether to
// sound + toast based on which page the viewer is on — chat-notify.js
// owns the /chat page, chat-events.js handles everywhere else.
//
// Listens on NewMsgBus + ChatNewSubject; edits / deletes / read
// receipts deliberately don't fire here.
func (h *Handler) GetEventsStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sse := render.NewSSE(w, r)

	var local <-chan struct{}
	var unsubscribe func()
	if h.NewMsgBus != nil {
		local, unsubscribe = h.NewMsgBus.Subscribe()
		defer unsubscribe()
	}

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.ChatNewSubject(h.cid(r.Context())), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe chat.new", "err", err)
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
		if err := sse.ExecuteScript(`window.fcChatPing && window.fcChatPing()`); err != nil {
			return
		}
	}
}

func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	msgID := r.URL.Query().Get("id")
	if msgID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.SoftDelete(r.Context(), msgID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sse := render.NewSSE(w, r)
	views, err := h.loadRecentFor(r.Context(), id.User.ID)
	if err == nil {
		_ = fatMorph(sse, views, true, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()))
	}
	h.broadcast(r.Context())
}

// MentionLimit caps how many suggestions the @mention popup shows. 7 was
// requested by the user; loose ceiling so the dropdown never grows past
// a thumb-friendly height on mobile.
const MentionLimit = 7

type mentionSignals struct {
	MentionQuery string `json:"mention_query"`
}

// GetMentionSearch renders the @mention typeahead popup as a Datastar
// patch. Reads the `mention_query` signal — the partial display-name
// token after the user's last `@` — and returns up to MentionLimit
// matches scoped to the current community. Empty / too-short query
// emits an empty popup (still patched so the dropdown closes cleanly
// after the user erases the @).
func (h *Handler) GetMentionSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in mentionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	var hits []auth.MemberHit
	if h.AuthRepo != nil {
		q := strings.TrimSpace(in.MentionQuery)
		if len(q) >= 1 {
			out, err := h.AuthRepo.SearchMembersByDisplayName(r.Context(), h.cid(r.Context()), q, MentionLimit)
			if err != nil {
				h.Log.Warn("mention search", "err", err)
			} else {
				hits = out
			}
		}
	}
	views := make([]webtempl.MentionHit, 0, len(hits))
	for _, h := range hits {
		views = append(views, webtempl.MentionHit{UserID: h.UserID, DisplayName: h.DisplayName})
	}
	_ = sse.PatchElementTempl(webtempl.MentionPopup(views))
}

func toMsgView(m Message) webtempl.MsgView {
	v := webtempl.MsgView{
		ID:               m.ID,
		AuthorID:         valueOrEmpty(m.AuthorID),
		AuthorName:       m.AuthorName,
		AuthorAvatar:     m.AuthorAvatar,
		Kind:             webtempl.MsgKind(m.Kind),
		BodyHTML:         m.BodyHTML,
		CreatedAt:        m.CreatedAt,
		Deleted:          m.IsDeleted(),
		PromotedThreadID: valueOrEmpty(m.PromotedThreadID),
		TitleSnippet:     render.AutoTitle(m.BodyMarkdown),
	}
	if m.ReplyTo != nil {
		v.ReplyTo = &webtempl.ReplySnippet{
			ID:         m.ReplyTo.ID,
			AuthorName: m.ReplyTo.AuthorName,
			Snippet:    m.ReplyTo.Snippet,
		}
	}
	return v
}

func toMsgViews(ms []Message) []webtempl.MsgView {
	out := make([]webtempl.MsgView, 0, len(ms))
	for _, m := range ms {
		out = append(out, toMsgView(m))
	}
	return out
}

func valueOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
