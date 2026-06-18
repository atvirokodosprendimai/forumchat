package chat

import (
	"context"
	"encoding/json"
	"errors"
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
	// ListProjects, if non-nil, returns the active projects in the
	// current community for the extract-to-project modal dropdown.
	// Set in main.go to avoid an import cycle with internal/projects.
	// nil → no projects → mod button rendered but the dropdown is
	// empty (which is fine — modal Save is gated on a non-empty pick).
	ListProjects  func(ctx context.Context, communityID string) []webtempl.ChatProjectView
	// Roster, when set, is pinged after a block/unblock so the presence
	// sidebar re-renders the viewer's data-blocked markers. Satisfied by
	// *presence.Tracker.Bump.
	Roster        RosterNotifier
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

// RosterNotifier wakes open presence sidebars so per-viewer markers
// (data-blocked) re-render after a block/unblock. Satisfied by
// *presence.Tracker.Bump. Optional — nil-safe.
type RosterNotifier interface {
	Bump(communityID string)
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

// loadRecentFor returns the latest N views, attaches the read-receipt
// list to the viewer's most recent own user message, and decorates
// every view with viewer-signed AttachmentView URLs. Receipts and
// signed URLs are viewer-specific so each connected SSE stream gets
// its own.
func (h *Handler) loadRecentFor(ctx context.Context, currentUserID string) ([]webtempl.MsgView, error) {
	msgs, err := h.Repo.Recent(ctx, h.cid(ctx), RecentLimit)
	if err != nil {
		return nil, err
	}
	blocked := h.blockedSet(ctx, currentUserID)
	views := make([]webtempl.MsgView, 0, len(msgs))
	for _, m := range msgs {
		// Per-viewer mute: drop blocked authors' messages from this
		// viewer's read model. System / thread-announce rows have an
		// empty AuthorID and are never blocked.
		if m.AuthorID != nil && *m.AuthorID != "" && blocked[*m.AuthorID] {
			continue
		}
		views = append(views, h.toMsgViewWith(m, currentUserID, h.cslug(ctx)))
	}
	h.attachReadReceipts(ctx, views, currentUserID)
	return views, nil
}

// blockedSet returns the set of user_ids the viewer has muted in this
// community. Empty/nil when no AuthRepo or no blocks.
func (h *Handler) blockedSet(ctx context.Context, currentUserID string) map[string]bool {
	if h.AuthRepo == nil || currentUserID == "" {
		return nil
	}
	ids, err := h.AuthRepo.ListBlocked(ctx, currentUserID, h.cid(ctx))
	if err != nil {
		h.Log.Warn("list blocked", "err", err)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
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
//  1. PatchElementTempl(#messages, Outer) → full latest-N list,
//     idiomorph-merged so existing image / video / iframe nodes that
//     were already loaded stay loaded.
//  2. PatchElementTempl(#chat-scroll-anchor, Replace) → swap the
//     anchor element for a fresh one, re-firing its data-init that
//     scrolls itself into view. Replace (not Outer) is essential —
//     idiomorph's same-id merge would keep the old anchor and
//     data-init would no-op.
func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool, currentUserID, viewerName, slug string) error {
	if err := sse.PatchElementTempl(
		webtempl.MessagesContainer(views, isMod, currentUserID, viewerName, slug),
		datastar.WithModeOuter(),
	); err != nil {
		return err
	}
	return sse.PatchElementTempl(
		webtempl.ChatScrollAnchor(),
		datastar.WithModeReplace(),
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

// parseAttachmentIDs decodes the JSON-encoded `attachment_ids` signal
// (a string in the Datastar bag — see sendSignals comment) into a
// slice, trims, deduplicates, and caps the count. Empty input → nil.
func parseAttachmentIDs(raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "[]" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return sanitiseAttachmentIDs(ids)
}

// sanitiseAttachmentIDs trims whitespace, drops empties, and caps the
// list at a small ceiling so a malicious / runaway client can't
// trigger a giant join in VerifyUploadsOwned. Order is preserved so
// the rendered grid follows the user's drop order.
func sanitiseAttachmentIDs(in []string) []string {
	const maxAttachments = 12
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= maxAttachments {
			break
		}
	}
	return out
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
	var projs []webtempl.ChatProjectView
	if h.ListProjects != nil {
		projs = h.ListProjects(r.Context(), h.cid(r.Context()))
	}
	_ = webtempl.ChatPage(webtempl.ChatPageData{
		Viewer:        h.viewer(r),
		IsMod:         id.Membership.Role.AtLeast(auth.RoleMod),
		CurrentUserID: id.User.ID,
		Messages:      views,
		Projects:      projs,
	}).Render(r.Context(), w)
}

type sendSignals struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id"`
	ImageData string `json:"image_data"`
	// AttachmentIDs is the JSON-encoded array of upload row ids the
	// composer staged via /chat/upload. Datastar treats array signals
	// as opaque from `data-bind`'d hidden inputs — value strings don't
	// round-trip back to arrays — so we keep this as a string in the
	// bag and json-decode it server-side. Empty / "" / "[]" all mean
	// "no attachments".
	AttachmentIDs string `json:"attachment_ids"`
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

	attIDs := parseAttachmentIDs(in.AttachmentIDs)
	if (body == "" && len(attIDs) == 0) || len(body) > 4000 {
		return
	}
	var replyTo *string
	if rid := strings.TrimSpace(in.ReplyToID); rid != "" {
		replyTo = &rid
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:   h.cid(r.Context()),
		AuthorID:      id.User.ID,
		BodyMarkdown:  body,
		ReplyToID:     replyTo,
		AttachmentIDs: attIDs,
	}); err != nil {
		h.Log.Error("send", "err", err)
		return
	}

	views, err := h.loadRecentFor(r.Context(), id.User.ID)
	if err == nil {
		_ = fatMorph(sse, views, id.Membership.Role.AtLeast(auth.RoleMod), id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()))
	}
	// Clear composer signals — including attachment_ids so the next
	// send starts with a fresh empty stage.
	_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":"","attachment_ids":""}`))

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

// PostUpload accepts a single multipart file from the chat composer
// and returns JSON describing the persisted upload. chat-attach.js
// fires this once per dropped / picked file via XHR so it can render
// progress per row. Returns 200 + JSON on success or a plain-text
// http.Error on failure (the JS path maps the body into a row-level
// error message).
func (h *Handler) PostUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if h.Uploads == nil {
		http.Error(w, "uploads disabled", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(h.Uploads.MaxSize + 1024); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	u, err := h.Uploads.Save(r.Context(), id.User.ID, h.cid(r.Context()),
		hdr.Header.Get("Content-Type"), hdr.Filename, file)
	if err != nil {
		switch {
		case errors.Is(err, uploads.ErrBadMIME):
			http.Error(w, "file type blocked", http.StatusUnsupportedMediaType)
		case errors.Is(err, uploads.ErrTooLarge):
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		default:
			h.Log.Error("chat upload", "err", err)
			http.Error(w, "upload failed", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       u.ID,
		"mime":     u.MIME,
		"kind":     MIMEKind(u.MIME),
		"size":     u.Size,
		"filename": u.Filename,
	})
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

// PostBlock mutes the target user (query param `user`) for the current
// viewer in this community. PostUnblock reverses it. Both re-render the
// actor's own chat immediately (blocked authors vanish from / return to
// their read model) and nudge the roster so the menu's Block/Unblock
// toggle flips.
func (h *Handler) PostBlock(w http.ResponseWriter, r *http.Request)   { h.setBlock(w, r, true) }
func (h *Handler) PostUnblock(w http.ResponseWriter, r *http.Request) { h.setBlock(w, r, false) }

func (h *Handler) setBlock(w http.ResponseWriter, r *http.Request, block bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	target := r.URL.Query().Get("user")
	if target == "" || target == id.User.ID {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	if h.AuthRepo == nil {
		http.Error(w, "blocking unavailable", http.StatusServiceUnavailable)
		return
	}
	cid := h.cid(r.Context())
	var err error
	if block {
		err = h.AuthRepo.BlockUser(r.Context(), id.User.ID, target, cid)
	} else {
		err = h.AuthRepo.UnblockUser(r.Context(), id.User.ID, target, cid)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sse := render.NewSSE(w, r)
	if views, lerr := h.loadRecentFor(r.Context(), id.User.ID); lerr == nil {
		isMod := id.Membership.Role.AtLeast(auth.RoleMod)
		_ = fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()))
	}
	if h.Roster != nil {
		h.Roster.Bump(cid)
	}
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

// toMsgViewWith builds a view AND attaches signed-URL-bearing
// attachment views for the given viewer. Used by loadRecentFor so the
// view-model carries everything the templ needs.
func (h *Handler) toMsgViewWith(m Message, viewerID, slug string) webtempl.MsgView {
	v := toMsgView(m)
	if len(m.Attachments) > 0 && h.Uploads != nil {
		out := make([]webtempl.AttachmentView, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			av := webtempl.AttachmentView{
				ID:       a.ID,
				URL:      h.Uploads.SignedURL(a.UploadID, viewerID, 24*time.Hour),
				MIME:     a.MIME,
				Kind:     a.Kind,
				Filename: a.Filename,
				Size:     a.Size,
			}
			if len(a.Extracts) > 0 {
				exs := make([]webtempl.ExtractView, 0, len(a.Extracts))
				for _, e := range a.Extracts {
					exs = append(exs, webtempl.ExtractView{
						ProjectID:   e.ProjectID,
						ProjectName: e.ProjectName,
						Mode:        e.Mode,
						IssueID:     e.IssueID,
						URL:         extractURL(slug, e),
					})
				}
				av.Extracts = exs
			}
			out = append(out, av)
		}
		v.Attachments = out
	}
	return v
}

// extractURL builds the per-badge anchor target.
func extractURL(slug string, e Extract) string {
	if e.Mode == "issue" && e.IssueID != "" {
		return "/c/" + slug + "/projects/" + e.ProjectID + "/issues/" + e.IssueID
	}
	return "/c/" + slug + "/projects/" + e.ProjectID + "/docs"
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
