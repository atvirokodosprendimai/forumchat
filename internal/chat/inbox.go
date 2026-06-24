package chat

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// InboxLimit caps the cross-community readonly inbox. The FE never holds more
// than this in the DOM (mirrors RecentLimit's intent).
const InboxLimit = 100

// MembershipScoper resolves which communities a viewer may read in the member
// inbox. Implemented by *auth.Repo (CommunityIDsForUser).
type MembershipScoper interface {
	CommunityIDsForUser(ctx context.Context, userID string, now int64) ([]string, error)
}

// InboxHandler serves the cross-community readonly chat feed shared by two
// surfaces:
//
//   - the SaaS member inbox (/chats): GodMode=false, scoped to the viewer's own
//     communities — "see your communities' chats, not everyone's".
//   - the self-hosted super-admin god-mode inbox (/superadmin/chat):
//     GodMode=true, every community + soft-deleted rows.
//
// One engine; the only axis of variation is the scope. Strictly readonly —
// there is no composer and no write path.
type InboxHandler struct {
	Repo *Repo
	// Bus fans in every community's writes in-process; the stream re-renders the
	// scoped feed on any signal. Nil-safe (the feed just won't live-update).
	Bus *Bus
	// NATS fans in cross-process writes via the community.*.chat wildcard.
	// Nil/disconnected → in-process Bus only.
	NATS *nats.Conn
	// Members resolves the viewer's community scope. Required when GodMode is
	// false; ignored (may be nil) for god-mode.
	Members MembershipScoper
	// GodMode true → every community + soft-deleted rows. False → scoped to the
	// viewer's memberships, deleted rows hidden.
	GodMode bool
	// StreamPath is the SSE endpoint the page opens on mount and re-opens on
	// reconnect ("/chats/stream" or "/superadmin/chat/stream").
	StreamPath string
}

// GetPage renders the inbox page; its data-init opens StreamPath which keeps
// the feed live.
func (h *InboxHandler) GetPage(w http.ResponseWriter, r *http.Request) {
	rows, err := h.feedRows(r.Context())
	if err != nil {
		http.Error(w, "load chats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := webtempl.SAChatPageData{
		Viewer:     inboxViewer(r),
		Rows:       rows,
		StreamPath: h.StreamPath,
		GodMode:    h.GodMode,
	}
	_ = webtempl.SAChatPage(data).Render(r.Context(), w)
}

// GetStream is the inbox SSE. It subscribes to the process-wide chat Bus plus
// the NATS community.*.chat wildcard and fat-morphs #sa-messages with the
// freshest SCOPED feed on any chat event anywhere. The scope is resolved once
// at (re)connect — memberships rarely change mid-stream, and a reconnect picks
// up any change. Readonly: it never writes.
func (h *InboxHandler) GetStream(w http.ResponseWriter, r *http.Request) {
	sse := render.NewSSE(w, r)

	// Initial sync on every (re)connect so a reconnecting client isn't stale.
	// force=true → scroll to newest on (re)connect.
	if rows, err := h.feedRows(r.Context()); err == nil {
		_ = inboxMorph(sse, rows, true)
	}

	var local <-chan string
	if h.Bus != nil {
		ch, unsubscribe := h.Bus.Subscribe()
		defer unsubscribe()
		local = ch
	}

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 64)
		sub, err := h.NATS.ChanSubscribe(natsx.AllChatSubject(), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
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
		rows, err := h.feedRows(r.Context())
		if err != nil {
			continue
		}
		// force=false → stick-to-bottom: only auto-scroll if the viewer is
		// already near the bottom, so scrolling up to read older messages isn't
		// yanked back when an unrelated chat event re-morphs the feed.
		if err := inboxMorph(sse, rows, false); err != nil {
			return
		}
	}
}

// scope resolves the community IDs to include. God-mode returns nil (every
// community). Member mode returns the viewer's approved, non-banned
// memberships — an empty slice means an empty inbox, never the god-mode feed.
func (h *InboxHandler) scope(ctx context.Context) ([]string, error) {
	if h.GodMode {
		return nil, nil
	}
	if h.Members == nil {
		return nil, nil
	}
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, nil
	}
	return h.Members.CommunityIDsForUser(ctx, id.User.ID, time.Now().Unix())
}

// feedRows resolves the viewer's current scope and loads the latest mapped
// rows — the read model reused by GetPage and GetStream. The scope is
// re-resolved on EVERY call (not captured once at connect), so a member banned
// or removed from a community mid-stream stops seeing its chat on the next
// event, not only after a reconnect. God-mode reads every community (deleted
// included); member mode reads the scope (deleted hidden). RecentGlobal /
// RecentForCommunities return newest-first; the inbox renders oldest→newest
// with the newest at the bottom, so we reverse here.
func (h *InboxHandler) feedRows(ctx context.Context) ([]webtempl.SAChatRow, error) {
	if h.Repo == nil {
		return nil, nil
	}
	var msgs []GlobalMessage
	var err error
	if h.GodMode {
		msgs, err = h.Repo.RecentGlobal(ctx, InboxLimit)
	} else {
		scope, serr := h.scope(ctx)
		if serr != nil {
			return nil, serr
		}
		msgs, err = h.Repo.RecentForCommunities(ctx, scope, InboxLimit, false)
	}
	if err != nil {
		return nil, err
	}
	out := make([]webtempl.SAChatRow, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		body := strings.TrimSpace(m.BodyHTML)
		text := ""
		if body == "" {
			// Some system rows carry no rendered HTML — fall back to the raw
			// markdown as text so the row isn't blank.
			text = strings.TrimSpace(m.BodyMarkdown)
		}
		out = append(out, webtempl.SAChatRow{
			ID:            m.ID,
			Community:     m.CommunityName,
			CommunitySlug: m.CommunitySlug,
			Channel:       m.ChannelSlug,
			Kind:          string(m.Kind),
			Author:        m.AuthorName,
			BodyHTML:      body,
			BodyText:      text,
			Deleted:       m.Deleted,
			CreatedAt:     m.CreatedAt.Format("Jan 2 15:04"),
			HRef:          inboxDeepLink(m),
		})
	}
	return out, nil
}

// inboxMorph outer-morphs the whole feed (idiomorph keeps #sa-messages the same
// node, preserving the viewer's scrollTop). Scroll-to-bottom is sticky: a
// pre-morph script records whether the viewer was near the bottom, and a
// post-morph script scrolls only if so (or if force — used on first connect).
func inboxMorph(sse *datastar.ServerSentEventGenerator, rows []webtempl.SAChatRow, force bool) error {
	if force {
		if err := sse.ExecuteScript(`window.__saStick = true`); err != nil {
			return err
		}
	} else {
		if err := sse.ExecuteScript(`window.__saStick = (() => { const f = document.getElementById('sa-messages'); return !f || (f.scrollHeight - f.scrollTop - f.clientHeight) < 120; })()`); err != nil {
			return err
		}
	}
	if err := sse.PatchElementTempl(webtempl.SAChatMessages(rows), datastar.WithModeOuter()); err != nil {
		return err
	}
	return sse.ExecuteScript(`(() => { const f = document.getElementById('sa-messages'); if (window.__saStick && f) { f.scrollTop = f.scrollHeight; } })()`)
}

// inboxDeepLink builds the deep link for a feed row. Thread-bound rows
// (promoted-to-thread, or thread announcements) jump to the forum thread;
// everything else opens the source channel's chat page.
func inboxDeepLink(m GlobalMessage) string {
	base := "/c/" + m.CommunitySlug
	if m.PromotedThreadID != nil && *m.PromotedThreadID != "" {
		return base + "/forum/" + *m.PromotedThreadID
	}
	if m.Kind == KindThreadAnnounce && m.RefThreadID != nil && *m.RefThreadID != "" {
		return base + "/forum/" + *m.RefThreadID
	}
	channel := m.ChannelSlug
	if channel == "" {
		channel = "general"
	}
	return base + "/chat/" + channel
}

// inboxViewer builds the topbar Viewer for the cross-community inbox from the
// session identity. CommunityName/Slug stay empty — this page is not bound to a
// single community.
func inboxViewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}
