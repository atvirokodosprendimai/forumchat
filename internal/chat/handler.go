package chat

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	Svc  *Service
	Repo *Repo
	NATS *nats.Conn
	Bus  *Bus
	// NewMsgBus carries ONLY "a new message just landed" events. The
	// global events SSE that powers cross-page notifications subscribes
	// here so it doesn't ping on edits, deletes, or read-receipt
	// updates (which still fire on Bus).
	NewMsgBus *Bus
	Uploads   *uploads.Store
	// AuthRepo is used to resolve @mentions in chat messages to user ids
	// before firing PushNotify. Optional — when nil, mention notifications
	// are skipped silently.
	AuthRepo *auth.Repo
	// PushNotify dispatches a web-push notification. Wired in main.go to
	// the push package's Sender so this package doesn't import push.
	// userIDs may be empty to broadcast across the whole community.
	PushNotify func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string)
	// RelayOut, if non-nil, fires-and-forgets a chat message to any matching
	// outbound webhooks. Wired in main.go to webhooks.Relay.Dispatch so this
	// package doesn't import webhooks. The normal user-send path calls it here;
	// slash-command output (/summary, /prompt) relays separately from main.go.
	// KindWebhook bot posts are never passed in — no echo loop.
	RelayOut func(communityID, channelID, authorName, bodyMD, channelName string)
	// ListProjects, if non-nil, returns the active projects in the
	// current community for the extract-to-project modal dropdown.
	// Set in main.go to avoid an import cycle with internal/projects.
	// nil → no projects → mod button rendered but the dropdown is
	// empty (which is fine — modal Save is gated on a non-empty pick).
	ListProjects func(ctx context.Context, communityID string) []webtempl.ChatProjectView
	// Summary runs the /summary slash command: summarise the channel's recent
	// history with an agent (in a public agent thread) and RETURN the recap for
	// an ephemeral panel shown only to the requester — like /search, it is NOT
	// posted to the channel. The user may then publish it with PublishSummary.
	// Wired in main.go to bridge chat → agent → chat without an import cycle.
	// nil when the Agent feature is disabled — the command is then ignored.
	Summary func(ctx context.Context, communityID, channelID, requesterID, requesterName string) SummaryResult
	// PublishSummary posts a previously-generated /summary into the channel as a
	// system message everyone sees. The summary is identified by its agent
	// thread id; the answer is re-read server-side from that thread (after
	// confirming it belongs to the requester) so the client can't inject
	// content. Returns false when nothing was posted. Wired in main.go; nil
	// when the Agent feature is disabled.
	PublishSummary func(ctx context.Context, communityID, channelID, threadID, requesterID, requesterName string) bool
	// Prompt runs the /prompt slash command: run a free-form prompt through an
	// agent in a new public thread and post the result (+ thread link) back to
	// the channel. Wired in main.go; nil when AI is disabled.
	Prompt func(ctx context.Context, communityID, channelID, requesterID, requesterName, prompt string)
	// Search runs the /search slash command: fused full-text + semantic search,
	// returning up to limit result views for the sender's ephemeral panel. Unlike
	// /summary and /prompt it is NOT posted to the channel — results are personal,
	// shown only to the sender. Wired in main.go; nil when search is unavailable.
	Search func(ctx context.Context, communityID, slug, query string, limit int) []webtempl.SearchResultView
	// Translate powers the interactive /translate composer typeahead: given the
	// text after "/translate", it returns up to 3 English translations (source
	// language auto-detected) for the live popup. Wired in main.go to the agent
	// package's Ollama-backed Translate using the TRANSLATE_* config — nil when
	// the feature is disabled (the popup then stays empty and closes).
	Translate func(ctx context.Context, text string) ([]string, error)
	// MentionAgents, when set, returns the community's in-chat agents as
	// mention hits (UserID = agent id, DisplayName = agent name) to union into
	// the @mention autocomplete so a member can address a bot by name. Wired in
	// main.go from agent.Repo; nil when AI is disabled.
	MentionAgents func(ctx context.Context, communityID string) []webtempl.MentionHit
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

// SummaryResult is the outcome of the /summary slash command: a channel recap
// generated into a (shared) agent thread and returned for the requester's
// ephemeral panel rather than posted. Publish it later via PublishSummary using
// ThreadID. On failure Err carries a user-facing message and ThreadID is empty.
type SummaryResult struct {
	ThreadID  string // agent thread holding the answer; "" on error
	BodyHTML  string // rendered summary markdown, for the panel
	ThreadURL string // relative deep link to the agent thread, "" if none
	Err       string // non-empty → render as the panel's error text
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

// activeChannel resolves the channel a request is scoped to. A non-empty
// route {channel} slug resolves that channel; an empty or unknown slug
// falls back to the community's undeletable #general. This keeps every
// read/write path channel-aware while a community that never makes a
// second channel behaves exactly as before.
func (h *Handler) activeChannel(ctx context.Context, slug string) (Channel, error) {
	cid := h.cid(ctx)
	if slug != "" {
		if ch, err := h.Repo.ChannelBySlug(ctx, cid, slug); err == nil {
			return ch, nil
		}
	}
	return h.Repo.DefaultChannel(ctx, cid)
}

// channelSlug pulls the {channel} route param (empty when the route has
// none — e.g. the legacy /chat path that lands on #general).
func channelSlug(r *http.Request) string { return chi.URLParam(r, "channel") }

// isSlashCommand reports whether body is the slash command /<cmd>, alone or
// followed by arguments.
func isSlashCommand(body, cmd string) bool {
	b := strings.ToLower(strings.TrimSpace(body))
	return b == "/"+cmd || strings.HasPrefix(b, "/"+cmd+" ")
}

// loadRecentFor returns the latest N views for channelID, attaches the
// read-receipt list to the viewer's most recent own user message, and
// decorates every view with viewer-signed AttachmentView URLs. Receipts
// and signed URLs are viewer-specific so each connected SSE stream gets
// its own.
func (h *Handler) loadRecentFor(ctx context.Context, channelID, currentUserID string) ([]webtempl.MsgView, error) {
	msgs, err := h.Repo.Recent(ctx, channelID, RecentLimit)
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
	h.attachReadReceipts(ctx, channelID, views, currentUserID)
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
func (h *Handler) attachReadReceipts(ctx context.Context, channelID string, views []webtempl.MsgView, currentUserID string) {
	if currentUserID == "" || h.Repo == nil {
		return
	}
	for i := range views {
		v := views[i]
		if v.Kind != webtempl.MsgKindUser || v.Deleted || v.AuthorID != currentUserID {
			continue
		}
		readers, err := h.Repo.ReadersSince(ctx, channelID, v.CreatedAt.Unix(), currentUserID, 30)
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
func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool, currentUserID, viewerName, slug, channelSlug string) error {
	if err := sse.PatchElementTempl(
		webtempl.MessagesContainer(views, isMod, currentUserID, viewerName, slug, channelSlug),
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
	// Welcome lands in #general (PostSystem → Insert default-channel
	// fallback). Broadcast empty channel id → every open stream refreshes
	// its active channel safely.
	if h.Bus != nil {
		h.Bus.Broadcast("")
	}
	if h.NewMsgBus != nil {
		h.NewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte(""))
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
func (h *Handler) broadcast(ctx context.Context, channelID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(channelID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.cid(ctx)), []byte(channelID))
	}
}

// broadcastNewMsg is broadcast() plus a fan-out on the strict
// "new-message-only" channel that the cross-page events stream
// listens on. Called from PostSend, Welcome, the forum bridge —
// anywhere a brand-new chat row appears. NOT from PostDelete or
// PostMarkRead.
func (h *Handler) broadcastNewMsg(ctx context.Context, channelID string) {
	h.broadcast(ctx, channelID)
	if h.NewMsgBus != nil {
		h.NewMsgBus.Broadcast(channelID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatNewSubject(h.cid(ctx)), []byte("new"))
	}
}

// PostSearchPublish publishes result(s) from the sender's ephemeral /search
// panel into the channel as a clickable system message everyone can see. The
// `search_idx` signal selects one result (>=0) or all of them (<0); the search
// is re-run server-side from the `search_q` signal so the message links are
// authoritative, not client-supplied.
func (h *Handler) PostSearchPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || h.Search == nil {
		return
	}
	var in struct {
		Query string `json:"search_q"`
		Idx   int    `json:"search_idx"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return
	}
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil || ch.IsArchived() {
		return
	}
	views := h.Search(r.Context(), h.cid(r.Context()), h.cslug(r.Context()), query, 6)
	if len(views) == 0 {
		return
	}

	name := id.Membership.DisplayName
	var shared []webtempl.SearchResultView
	if in.Idx >= 0 {
		if in.Idx >= len(views) {
			return
		}
		shared = views[in.Idx : in.Idx+1]
	} else {
		shared = views
	}
	if _, err := h.Svc.PostSystemHTMLToChannel(r.Context(), h.cid(r.Context()), ch.ID, publishSearchHTML(name, query, shared)); err != nil {
		h.Log.Error("search publish", "err", err)
		return
	}
	h.broadcastNewMsg(r.Context(), ch.ID)

	// Relay to outbound webhooks. This bypasses chat.PostSend, so fire it here
	// with a plain-text body (the channel message itself is trusted HTML, which
	// Slack/Discord would render literally). No-op when webhooks are off.
	if h.RelayOut != nil {
		h.RelayOut(h.cid(r.Context()), ch.ID, name, publishSearchText(query, shared), ch.Name)
	}

	sse := render.NewSSE(w, r)
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	if msgs, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID); err == nil {
		_ = fatMorph(sse, msgs, isMod, id.User.ID, name, h.cslug(r.Context()), ch.Slug)
	}
}

// PostSummaryPublish posts a previously-generated /summary into the channel as a
// system message everyone sees. The summary is identified by the
// `summary_thread_id` signal and re-read server-side from its agent thread (so
// the client can't inject content); PublishSummary verifies ownership. The
// sender's ephemeral panel is then cleared and the channel re-rendered.
func (h *Handler) PostSummaryPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || h.PublishSummary == nil {
		return
	}
	var in struct {
		ThreadID string `json:"summary_thread_id"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		return
	}
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil || ch.IsArchived() {
		return
	}
	if !h.PublishSummary(r.Context(), h.cid(r.Context()), ch.ID, threadID, id.User.ID, id.Membership.DisplayName) {
		return
	}

	sse := render.NewSSE(w, r)
	// Clear the ephemeral panel + its stashed thread id, then re-render the
	// channel so the published summary appears for the sender too (the chat
	// stream is torn down by this @post — same as a normal send).
	_ = sse.PatchSignals([]byte(`{"summary_thread_id":""}`))
	_ = sse.ExecuteScript(`const w=document.querySelector('#chat-search'); if(w){w.className='';w.replaceChildren();}`)
	if msgs, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID); err == nil {
		_ = fatMorph(sse, msgs, id.Membership.Role.AtLeast(auth.RoleMod), id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug)
	}
}

// publishSearchHTML renders a system-message body as TRUSTED HTML: a header
// crediting the sharer plus one real anchor per result. We build HTML directly
// (not markdown) because the user-markdown pipeline's "no hidden URLs" rewrite
// would strip our friendly link labels down to bare, non-clickable paths. All
// user-derived text (name, query, label) is HTML-escaped; the URLs are
// internally built by the search link resolver. Single- and all-result share
// use the same shape (the caller slices to one).
func publishSearchHTML(name, query string, views []webtempl.SearchResultView) string {
	var b strings.Builder
	b.WriteString("🔎 <strong>")
	b.WriteString(html.EscapeString(name))
	b.WriteString("</strong> shared ")
	if len(views) == 1 {
		b.WriteString("a result")
	} else {
		b.WriteString("search results")
	}
	b.WriteString(" for “")
	b.WriteString(html.EscapeString(query))
	b.WriteString("”:<ul class=\"chat-search-shared\">")
	for _, v := range views {
		b.WriteString("<li><a href=\"")
		b.WriteString(html.EscapeString(v.URL))
		b.WriteString("\">")
		b.WriteString(html.EscapeString(publishLinkText(v)))
		b.WriteString("</a></li>")
	}
	b.WriteString("</ul>")
	return b.String()
}

// publishSearchText renders the shared search results as a plain-text body for
// the outbound webhook relay — "Title — url" per line. The relay credits the
// sharer separately (author field), so this body omits the name. Kept parallel
// to publishSearchHTML (which is the in-channel HTML); single- and all-result
// share use the same shape (the caller slices to one).
func publishSearchText(query string, views []webtempl.SearchResultView) string {
	var b strings.Builder
	b.WriteString("🔎 shared ")
	if len(views) == 1 {
		b.WriteString("a result")
	} else {
		b.WriteString("search results")
	}
	b.WriteString(" for “")
	b.WriteString(query)
	b.WriteString("”:")
	for _, v := range views {
		b.WriteString("\n• ")
		b.WriteString(publishLinkText(v))
		b.WriteString(" — ")
		b.WriteString(v.URL)
	}
	return b.String()
}

// publishLinkText is the link label — the result title, or a kind-aware fallback
// when the hit is a bodied row without a subject (mirrors webtempl.searchResultTitle).
func publishLinkText(v webtempl.SearchResultView) string {
	if t := strings.TrimSpace(v.Title); t != "" {
		return t
	}
	switch v.Kind {
	case "chat":
		return "Chat message"
	case "post":
		return "Forum reply"
	case "issue_comment":
		return "Issue comment"
	case "discussion_reply":
		return "Discussion reply"
	case "ai":
		return "Agent message"
	}
	return "Result"
}

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil {
		http.Error(w, "load channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	views, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID)
	if err != nil {
		http.Error(w, "load chat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	channels, unread, _ := h.channelViews(r.Context(), id.User.ID)
	// The active channel is "read" the moment it's rendered — drop its dot.
	delete(unread, ch.ID)
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
		Channels:      channels,
		ActiveID:      ch.ID,
		ActiveSlug:    ch.Slug,
		ActiveTopic:   ch.Topic,
		Unread:        unread,
		CanManage:     id.Membership.Role.AtLeast(auth.RoleMod),
	}).Render(r.Context(), w)
}

// channelViews assembles the switcher's channel list + the viewer's
// unread-dot set for the current community.
func (h *Handler) channelViews(ctx context.Context, userID string) ([]webtempl.ChannelView, map[string]bool, error) {
	cid := h.cid(ctx)
	chans, err := h.Repo.ListChannels(ctx, cid, false)
	if err != nil {
		return nil, map[string]bool{}, err
	}
	out := make([]webtempl.ChannelView, 0, len(chans))
	for _, c := range chans {
		out = append(out, webtempl.ChannelView{
			ID: c.ID, Slug: c.Slug, Name: c.Name, Topic: c.Topic, IsDefault: c.IsDefault,
		})
	}
	unread, err := h.Repo.UnreadChannels(ctx, cid, userID)
	if err != nil {
		unread = map[string]bool{}
	}
	return out, unread, nil
}

// GetChatRedirect sends bare /chat to the community's #general so the URL
// always carries a channel slug.
func (h *Handler) GetChatRedirect(w http.ResponseWriter, r *http.Request) {
	ch, err := h.Repo.DefaultChannel(r.Context(), h.cid(r.Context()))
	slug := "general"
	if err == nil {
		slug = ch.Slug
	}
	http.Redirect(w, r, "/c/"+h.cslug(r.Context())+"/chat/"+slug, http.StatusSeeOther)
}

// channelFormSignals carries the create / edit dialog fields plus the
// actor's current active channel (for the switcher highlight on re-render).
type channelFormSignals struct {
	ChannelID string `json:"_ch_edit_id"`
	Name      string `json:"ch_name"`
	Topic     string `json:"ch_topic"`
	ActiveID  string `json:"active_channel"`
}

// requireManage gates channel CRUD to mod/admin.
func (h *Handler) requireManage(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok || !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return auth.Identity{}, false
	}
	return id, true
}

// channelErrMsg maps the service's typed channel errors to friendly copy.
func channelErrMsg(err error) string {
	switch {
	case errors.Is(err, ErrEmptyChannelName):
		return "Channel name required."
	case errors.Is(err, ErrReservedSlug):
		return "“general” is reserved."
	case errors.Is(err, ErrChannelCap):
		return "Channel limit reached (max 10)."
	case errors.Is(err, ErrSlugTaken):
		return "A channel with that name already exists."
	case errors.Is(err, ErrDefaultChannel):
		return "The #general channel can't be changed."
	default:
		return "Couldn't save the channel."
	}
}

// afterChannelChange re-renders the actor's switcher, closes the dialogs,
// and broadcasts an empty (structural) signal so every other open chat
// stream re-renders its switcher + active channel.
func (h *Handler) afterChannelChange(ctx context.Context, sse *datastar.ServerSentEventGenerator, userID, activeID string) {
	channels, unread, _ := h.channelViews(ctx, userID)
	delete(unread, activeID)
	_ = sse.PatchElementTempl(webtempl.ChannelSwitcher(h.cslug(ctx), channels, activeID, true))
	_ = sse.PatchSignals([]byte(`{"_ch_create_open":false,"_ch_edit_open":false,"ch_name":"","ch_topic":""}`))
	h.broadcast(ctx, "")
}

func (h *Handler) PostCreateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireManage(w, r)
	if !ok {
		return
	}
	var in channelFormSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if _, err := h.Svc.CreateChannel(r.Context(), h.cid(r.Context()), id.User.ID, in.Name, in.Topic); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("chan-error", channelErrMsg(err)))
		return
	}
	h.afterChannelChange(r.Context(), sse, id.User.ID, in.ActiveID)
}

func (h *Handler) PostRenameChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireManage(w, r)
	if !ok {
		return
	}
	var in channelFormSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if _, err := h.Svc.RenameChannel(r.Context(), h.cid(r.Context()), in.ChannelID, in.Name); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("chan-error", channelErrMsg(err)))
		return
	}
	h.afterChannelChange(r.Context(), sse, id.User.ID, in.ActiveID)
}

func (h *Handler) PostSetTopic(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireManage(w, r)
	if !ok {
		return
	}
	var in channelFormSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Svc.SetTopic(r.Context(), h.cid(r.Context()), in.ChannelID, in.Topic); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("chan-error", channelErrMsg(err)))
		return
	}
	h.afterChannelChange(r.Context(), sse, id.User.ID, in.ActiveID)
}

func (h *Handler) PostArchiveChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireManage(w, r)
	if !ok {
		return
	}
	var in channelFormSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Svc.Archive(r.Context(), h.cid(r.Context()), in.ChannelID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("chan-error", channelErrMsg(err)))
		return
	}
	h.afterChannelChange(r.Context(), sse, id.User.ID, in.ActiveID)
}

// PostDeleteChannel hard-deletes a channel. Admin-only (stricter than
// the mod-gated create/rename/archive).
func (h *Handler) PostDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || !id.Membership.Role.AtLeast(auth.RoleAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in channelFormSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Svc.Delete(r.Context(), h.cid(r.Context()), in.ChannelID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("chan-error", channelErrMsg(err)))
		return
	}
	h.afterChannelChange(r.Context(), sse, id.User.ID, in.ActiveID)
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
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil {
		h.Log.Error("send: resolve channel", "err", err)
		return
	}
	if ch.IsArchived() {
		return // archived channels are read-only
	}
	// Slash commands. /summary summarises the channel's recent history with an
	// agent and shows the recap in an ephemeral panel for the SENDER only (like
	// /search) — it is NOT stored or broadcast. The user may publish it to the
	// channel from the panel. Only when the Agent feature wired the bridge. The
	// generation runs on the request context: if the sender navigates away it
	// cancels (the recap would have nowhere to render anyway).
	if h.Summary != nil && isSlashCommand(body, "summary") {
		_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":"","attachment_ids":""}`))
		// Show a spinner immediately — the summary takes a few seconds and the
		// composer has already cleared, so without this the UI looks frozen. The
		// SDK flushes per event, so this lands before the slow call below.
		_ = sse.PatchElementTempl(webtempl.ChatPanelLoading("🧠 Summarising the channel…"))
		res := h.Summary(r.Context(), h.cid(r.Context()), ch.ID, id.User.ID, id.Membership.DisplayName)
		_ = sse.PatchElementTempl(webtempl.ChatSummaryPanel(h.cslug(r.Context()), ch.Slug, res.ThreadID, res.BodyHTML, res.ThreadURL, res.Err))
		return
	}
	// /prompt <text> — run a free-form prompt through an agent in a new public
	// thread. Posts a "working" placeholder immediately, then the result.
	if h.Prompt != nil && isSlashCommand(body, "prompt") {
		cid := h.cid(r.Context())
		chID := ch.ID
		rid := id.User.ID
		rname := id.Membership.DisplayName
		isMod := id.Membership.Role.AtLeast(auth.RoleMod)
		prompt := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "/prompt"))
		_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":"","attachment_ids":""}`))
		if prompt == "" {
			return
		}
		// Immediate "working" placeholder — visible to the sender (this fatMorph)
		// and everyone else (the broadcast).
		_, _ = h.Svc.PostSystemMarkdown(r.Context(), cid, chID, "🤖 Working on your prompt… _(requested by "+rname+")_")
		h.broadcastNewMsg(r.Context(), chID)
		if views, err := h.loadRecentFor(r.Context(), chID, rid); err == nil {
			_ = fatMorph(sse, views, isMod, rid, rname, h.cslug(r.Context()), ch.Slug)
		}
		done := make(chan struct{})
		go func() { h.Prompt(context.Background(), cid, chID, rid, rname, prompt); close(done) }()
		select {
		case <-done:
		case <-r.Context().Done():
			return
		}
		if views, err := h.loadRecentFor(r.Context(), chID, rid); err == nil {
			_ = fatMorph(sse, views, isMod, rid, rname, h.cslug(r.Context()), ch.Slug)
		}
		return
	}
	// /search <query> — fused full-text + semantic search. Renders an ephemeral
	// results panel for the SENDER only: not stored as a message, not broadcast
	// (search results are personal — posting them would spam the channel).
	if h.Search != nil && isSlashCommand(body, "search") {
		query := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "/search"))
		_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":"","attachment_ids":""}`))
		if query == "" {
			return
		}
		// Show a spinner immediately — search can take a couple of seconds and the
		// composer has already cleared. The SDK flushes per event, so this lands
		// before the (slow) search below; the results then morph it in place.
		_ = sse.PatchElementTempl(webtempl.ChatPanelLoading("🔍 Searching “" + query + "”…"))
		views := h.Search(r.Context(), h.cid(r.Context()), h.cslug(r.Context()), query, 6)
		// Stash the query in a signal so the panel's share buttons can re-run it
		// server-side on publish (JSON avoids any URL-escaping pitfalls).
		if payload, err := json.Marshal(map[string]string{"search_q": query}); err == nil {
			_ = sse.PatchSignals(payload)
		}
		_ = sse.PatchElementTempl(webtempl.ChatSearchResults(h.cslug(r.Context()), ch.Slug, query, views))
		return
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:   h.cid(r.Context()),
		ChannelID:     ch.ID,
		AuthorID:      id.User.ID,
		BodyMarkdown:  body,
		ReplyToID:     replyTo,
		AttachmentIDs: attIDs,
	}); err != nil {
		h.Log.Error("send", "err", err)
		return
	}

	views, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID)
	if err == nil {
		_ = fatMorph(sse, views, id.Membership.Role.AtLeast(auth.RoleMod), id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug)
	}
	// Clear composer signals — including attachment_ids so the next
	// send starts with a fresh empty stage.
	_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":"","attachment_ids":""}`))

	h.broadcastNewMsg(r.Context(), ch.ID)

	// Relay to outbound webhooks (Slack/Discord/generic). Fire-and-forget;
	// the callback detaches from the request. Human messages only.
	if h.RelayOut != nil {
		h.RelayOut(h.cid(r.Context()), ch.ID, id.Membership.DisplayName, body, ch.Name)
	}

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

type forwardSignals struct {
	SourceID      string `json:"fwd_source_id"`
	TargetChannel string `json:"fwd_target_channel"` // target channel slug
	Note          string `json:"fwd_note"`
}

// PostForward forwards a chat message into another channel (Discord-style).
// The forwarded copy carries a "Forwarded from #channel" embed, the
// optional note as its body, and re-linked copies of the source's
// attachments. Open to any member — every channel is public. Redirects to
// the target channel so the forwarder sees the message land.
func (h *Handler) PostForward(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in forwardSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	srcID := strings.TrimSpace(in.SourceID)
	targetSlug := strings.TrimSpace(in.TargetChannel)
	if srcID == "" || targetSlug == "" || len(in.Note) > 4000 {
		return
	}
	cid := h.cid(r.Context())
	target, err := h.Repo.ChannelBySlug(r.Context(), cid, targetSlug)
	if err != nil {
		h.Log.Warn("forward: resolve target channel", "err", err)
		return
	}
	if target.IsArchived() {
		return // archived channels are read-only
	}
	if _, err := h.Svc.Forward(r.Context(), ForwardInput{
		CommunityID:     cid,
		TargetChannelID: target.ID,
		AuthorID:        id.User.ID,
		Note:            in.Note,
		SourceMsgID:     srcID,
	}); err != nil {
		h.Log.Error("forward", "err", err)
		return
	}
	// Wake the target channel's open streams + cross-page dots, then send
	// the forwarder there so they see it land.
	h.broadcastNewMsg(r.Context(), target.ID)

	// Relay to outbound webhooks — forwarding lands real content in the target
	// channel but bypasses chat.PostSend. The forwarded copy's own text is the
	// optional note; fall back to a marker so an empty-note forward isn't a
	// silent relay. No-op when webhooks are off.
	if h.RelayOut != nil {
		relayBody := strings.TrimSpace(in.Note)
		if relayBody == "" {
			relayBody = "↪ forwarded a message"
		}
		h.RelayOut(cid, target.ID, id.Membership.DisplayName, relayBody, target.Name)
	}

	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/chat/" + target.Slug)
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
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil {
		h.Log.Warn("mark read: resolve channel", "err", err)
		return
	}
	if err := h.Repo.MarkRead(r.Context(), id.User.ID, cid, ch.ID, strings.TrimSpace(in.LastID), time.Now()); err != nil {
		h.Log.Warn("mark read", "err", err)
		return
	}
	if !h.shouldBroadcastRead(ch.ID, id.User.ID, time.Now()) {
		return
	}
	h.broadcast(r.Context(), ch.ID)
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
	ch, err := h.activeChannel(r.Context(), channelSlug(r))
	if err != nil {
		http.Error(w, "load channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)

	// Initial sync: on every (re)connection — including when the browser
	// re-establishes SSE after tab sleep — push the latest 100 immediately.
	// Without this, a reconnecting client would see stale messages until the
	// next chat event fires.
	if views, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID); err == nil {
		_ = fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug)
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
		var changed string
		select {
		case <-r.Context().Done():
			return
		case changed = <-local:
			// in-process bus carries the changed channel id
		case msg, ok := <-natsCh:
			if !ok {
				natsCh = nil
				continue
			}
			changed = string(msg.Data)
		}
		// A different channel changed: light its switcher unread dot and
		// leave this viewer's #messages untouched.
		if changed != "" && changed != ch.ID {
			if err := pushUnreadDot(sse, changed); err != nil {
				return
			}
			continue
		}
		// Empty changed id = structural change (channel created / renamed /
		// archived / deleted, or a bridge message) — re-render the switcher
		// so the new shape appears live, then fall through to refresh the
		// active channel's messages.
		if changed == "" {
			if channels, _, cerr := h.channelViews(r.Context(), id.User.ID); cerr == nil {
				_ = sse.PatchElementTempl(webtempl.ChannelSwitcher(h.cslug(r.Context()), channels, ch.ID, isMod))
			}
		}
		views, err := h.loadRecentFor(r.Context(), ch.ID, id.User.ID)
		if err != nil {
			continue
		}
		if err := fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug); err != nil {
			return
		}
	}
}

// pushUnreadDot merges a single channel's unread flag into the client's
// chat_unread map signal (Datastar deep-merges nested signal objects, so
// other channels' dots are preserved). channelID is a DB-minted id
// (uuid / hex), safe to inline without escaping.
func pushUnreadDot(sse *datastar.ServerSentEventGenerator, channelID string) error {
	return sse.PatchSignals([]byte(`{"chat_unread":{"` + channelID + `":true}}`))
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

	// The cross-page events stream is channel-agnostic — it pings on ANY
	// new message regardless of which channel it landed in, so it ignores
	// the channel id the bus carries.
	var local <-chan string
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
	// Resolve the message's own channel (the one the mod is viewing) so
	// the re-render + broadcast target it — delete is a community-level
	// route with no {channel} in the URL.
	msg, err := h.Repo.ByID(r.Context(), msgID)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if err := h.Repo.SoftDelete(r.Context(), msgID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sse := render.NewSSE(w, r)
	if ch, cerr := h.Repo.ChannelByID(r.Context(), msg.ChannelID); cerr == nil {
		if views, lerr := h.loadRecentFor(r.Context(), ch.ID, id.User.ID); lerr == nil {
			_ = fatMorph(sse, views, true, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug)
		}
	}
	h.broadcast(r.Context(), msg.ChannelID)
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
	if ch, cerr := h.activeChannel(r.Context(), channelSlug(r)); cerr == nil {
		if views, lerr := h.loadRecentFor(r.Context(), ch.ID, id.User.ID); lerr == nil {
			isMod := id.Membership.Role.AtLeast(auth.RoleMod)
			_ = fatMorph(sse, views, isMod, id.User.ID, id.Membership.DisplayName, h.cslug(r.Context()), ch.Slug)
		}
	}
	if h.Roster != nil {
		h.Roster.Bump(cid)
	}
}

type reportSignals struct {
	Reason string `json:"report_reason"`
}

// PostReport files a moderation report against the target user (query
// param `user`) with the `report_reason` signal. Any approved member can
// report; mods see open reports in /admin. Confirms via the global toast.
func (h *Handler) PostReport(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in reportSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	target := r.URL.Query().Get("user")
	reason := strings.TrimSpace(in.Reason)
	if target == "" || target == id.User.ID || reason == "" {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if h.AuthRepo == nil {
		http.Error(w, "reporting unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.AuthRepo.CreateUserReport(r.Context(), uuid.NewString(), id.User.ID, target, h.cid(r.Context()), reason, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"report_reason":"","_ctx_report_open":false,"_pm_toast_text":"Thanks — moderators have been notified","_pm_toast_href":""}`))
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
	// Union in chat-agent names matching the query so a member can @mention a
	// bot. Bots come first (small set) and the whole list is capped.
	if h.MentionAgents != nil {
		q := strings.ToLower(strings.TrimSpace(in.MentionQuery))
		var bots []webtempl.MentionHit
		for _, a := range h.MentionAgents(r.Context(), h.cid(r.Context())) {
			if q == "" || strings.HasPrefix(strings.ToLower(a.DisplayName), q) {
				bots = append(bots, a)
			}
		}
		views = append(bots, views...)
		if len(views) > MentionLimit {
			views = views[:MentionLimit]
		}
	}
	_ = sse.PatchElementTempl(webtempl.MentionPopup(views))
}

type translateSignals struct {
	TranslateQuery string `json:"translate_query"`
}

// GetTranslate renders the /translate typeahead popup as a Datastar patch.
// Reads the `translate_query` signal — the text after "/translate " that the
// composer detector extracted — translates it to English (source language
// auto-detected) and returns up to 3 alternatives. It also patches
// `_translate_open` to whether there are any rows, so an empty result (feature
// disabled, blank query, or a provider error) closes the popup cleanly and the
// composer's Enter falls back to a normal send.
func (h *Handler) GetTranslate(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in translateSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	var opts []string
	if q := strings.TrimSpace(in.TranslateQuery); h.Translate != nil && q != "" {
		out, err := h.Translate(r.Context(), q)
		if err != nil {
			h.Log.Warn("translate", "err", err)
		} else {
			opts = out
		}
	}
	_ = sse.PatchElementTempl(webtempl.TranslatePopup(opts))
	if len(opts) > 0 {
		_ = sse.PatchSignals([]byte(`{"_translate_open":true}`))
	} else {
		_ = sse.PatchSignals([]byte(`{"_translate_open":false}`))
	}
}

func toMsgView(m Message) webtempl.MsgView {
	v := webtempl.MsgView{
		ID:               m.ID,
		AuthorID:         valueOrEmpty(m.AuthorID),
		AuthorName:       m.AuthorName,
		AuthorAvatar:     m.AuthorAvatar,
		Kind:             webtempl.MsgKind(m.Kind),
		BodyHTML:         m.BodyHTML,
		GenStatus:        m.GenStatus,
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
	if m.ForwardedFrom != nil {
		v.Forwarded = &webtempl.ForwardView{
			ChannelSlug: m.ForwardedFrom.ChannelSlug,
			ChannelName: m.ForwardedFrom.ChannelName,
			AuthorName:  m.ForwardedFrom.AuthorName,
			Snippet:     m.ForwardedFrom.Snippet,
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

func valueOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
