package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

const (
	sessKeyGuestID     = "rooms_guest_id"
	sessKeyGuestName   = "rooms_guest_name"
	sessKeyGuestRoomID = "rooms_guest_room_id"
)

type Handler struct {
	Svc        *Service
	Repo       *Repo
	Bus        *Bus
	State      *State
	AuthRepo   *auth.Repo
	Sessions   *scs.SessionManager
	Log        *slog.Logger
	IceServers []ICEServer // optional STUN/TURN config; client-side passes to RTCPeerConnection

	// ChatSvc + ChatRepo + ChatBus are optional. When wired, "Share to
	// chat" inserts a join-link message into the admin's community common
	// chat. We use ChatRepo directly (bypassing the markdown sanitizer) so
	// the link renders as a real <a target="_blank"> instead of being
	// flattened to plain text by the Discord-style URL guard.
	ChatSvc  *chat.Service
	ChatRepo *chat.Repo
	ChatBus  *chat.Bus
}

// ICEServer matches the WebRTC dictionary shape.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// AuthRoutes mounts the slice that requires a logged-in user: the grid and
// the admin-only invite link operations. Per-room interaction routes are
// mounted via OpenRoutes so guests (who arrive through a share-link cookie,
// not an account) can reach them.
func (h *Handler) AuthRoutes(r chi.Router) {
	r.Get("/rooms", h.GetGrid)
	r.Post("/rooms/{id}/invite", h.PostCreateInvite)
	r.Post("/rooms/{id}/invite/revoke", h.PostRevokeInvite)
}

// OpenRoutes mounts the routes that an auth user OR a session-scoped guest
// may use. Each handler defers identity resolution to caller(), which
// accepts either an SCS-backed auth session or the guest keys set by
// PostInviteJoin.
func (h *Handler) OpenRoutes(r chi.Router) {
	r.Get("/rooms/{id}", h.GetRoom)
	r.Get("/rooms/{id}/stream", h.GetStream)
	r.Post("/rooms/{id}/signal/send", h.PostSignal)
	r.Post("/rooms/{id}/join", h.PostJoin)
	r.Post("/rooms/{id}/leave", h.PostLeave)
	r.Post("/rooms/{id}/ping", h.PostPing)
	r.Post("/rooms/{id}/approve", h.PostApprove)
	r.Post("/rooms/{id}/decline", h.PostDecline)
	r.Post("/rooms/{id}/promote", h.PostPromote)
	r.Post("/rooms/{id}/public", h.PostTogglePublic)
	r.Post("/rooms/{id}/rename", h.PostRename)
	r.Post("/rooms/{id}/chat", h.PostChat)
	r.Post("/rooms/{id}/share-to-chat", h.PostShareToChat)
}

// PublicRoutes mounts the guest invite landing. No auth required.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/rooms/invite/{token}", h.GetInviteLanding)
	r.Post("/rooms/invite/{token}/join", h.PostInviteJoin)
}

// caller resolves the requester to an Identity — either an auth user, or
// a session-scoped guest. Returns (id, ok). `roomID` constrains guests to
// the room their invite belongs to.
func (h *Handler) caller(r *http.Request, roomID string) (Identity, bool) {
	uid := auth.CurrentUserID(r.Context(), h.Sessions)
	if uid != "" {
		name := h.resolveAuthName(r.Context(), uid)
		return Identity{UserID: uid, Name: name}, true
	}
	gid := h.Sessions.GetString(r.Context(), sessKeyGuestID)
	groom := h.Sessions.GetString(r.Context(), sessKeyGuestRoomID)
	if gid != "" && groom == roomID {
		name := h.Sessions.GetString(r.Context(), sessKeyGuestName)
		return Identity{GuestID: gid, Name: name}, true
	}
	return Identity{}, false
}

func (h *Handler) resolveAuthName(ctx context.Context, uid string) string {
	if n, err := h.Repo.displayNameForUser(ctx, uid); err == nil && n != "" {
		return n
	}
	if u, err := h.AuthRepo.UserByID(ctx, uid); err == nil {
		return u.Email
	}
	return uid
}

func (h *Handler) GetGrid(w http.ResponseWriter, r *http.Request) {
	uid := auth.CurrentUserID(r.Context(), h.Sessions)
	if uid == "" {
		http.Redirect(w, r, "/login?next=/rooms", http.StatusSeeOther)
		return
	}
	rooms, err := h.Repo.ListRooms(r.Context())
	if err != nil {
		h.Log.Error("rooms list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]webtempl.RoomsGridRow, 0, len(rooms))
	for _, rm := range rooms {
		snap := h.State.Snapshot(rm.ID)
		adminName := ""
		if snap.AdminKey != "" {
			for _, m := range snap.Members {
				if m.Key() == snap.AdminKey {
					adminName = m.Name
					break
				}
			}
		}
		rows = append(rows, webtempl.RoomsGridRow{
			ID:           rm.ID,
			Slot:         rm.Slot,
			Name:         rm.Name,
			IsPublic:     rm.IsPublic,
			MemberCount:  snap.MemberCount,
			PendingCount: snap.PendingCount,
			AdminName:    adminName,
		})
	}
	v := h.layoutViewer(r)
	_ = webtempl.RoomsGrid(webtempl.RoomsGridData{Viewer: v, Rows: rows}).Render(r.Context(), w)
}

func (h *Handler) GetRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	rm, err := h.Repo.RoomByID(r.Context(), roomID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Redirect(w, r, "/login?next=/rooms/"+roomID, http.StatusSeeOther)
		return
	}

	// Make sure the caller is admitted (no-op if already in). Auth users
	// follow the room policy; guests must already be admitted via JoinGuest
	// (they couldn't reach here without the invite cookie).
	if id.UserID != "" {
		if _, err := h.Svc.JoinAuth(r.Context(), roomID, id.UserID, id.Name); err != nil &&
			!errors.Is(err, ErrRoomFull) {
			h.Log.Warn("rooms auth-join", "err", err)
		}
	}

	snap := h.State.Snapshot(roomID)
	chat, err := h.Repo.ListChat(r.Context(), roomID, 200)
	if err != nil {
		h.Log.Warn("rooms chat list", "err", err)
	}
	inviteURL := ""
	if id.UserID != "" && snap.AdminKey == id.Key() {
		if inv, err := h.Repo.ActiveInviteForRoom(r.Context(), roomID); err == nil {
			scheme, host := publicSchemeHost(r)
			inviteURL = scheme + "://" + host + "/rooms/invite/" + inv.Token
		}
	}
	iceJSON := "[]"
	if h.IceServers != nil {
		if b, err := json.Marshal(h.IceServers); err == nil {
			iceJSON = string(b)
		}
	}

	v := h.layoutViewer(r)
	data := webtempl.RoomPageData{
		Viewer:         v,
		RoomID:         rm.ID,
		RoomName:       rm.Name,
		IsPublic:       rm.IsPublic,
		IsAdmin:        snap.AdminKey == id.Key() && !id.IsGuest(),
		IsGuest:        id.IsGuest(),
		MyKey:          id.Key(),
		MyName:         id.Name,
		Members:        toParticipantViews(snap.Members, snap.AdminKey, id.Key()),
		Pending:        toParticipantViews(snap.Pending, "", id.Key()),
		Chat:           toChatViews(chat, id.UserID, id.GuestID),
		InviteURL:      inviteURL,
		VideoCapHit:    snap.MemberCount > VideoCap,
		HasIceServers:  len(h.IceServers) > 0,
		IceServersJSON: iceJSON,
	}
	_ = webtempl.RoomPage(data).Render(r.Context(), w)
}

// GetStream is the long-lived per-room SSE for presence/chat/meta deltas.
// Datastar-style: each event re-renders the affected fragment.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	rm, err := h.Repo.RoomByID(r.Context(), roomID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sse := datastar.NewSSE(w, r)
	events, unsub := h.Bus.SubscribeRoom(roomID)
	defer unsub()

	// Signaling envelopes ride the SAME SSE stream as room events. Folding
	// them in here costs nothing on the server but saves the browser one
	// long-lived HTTP connection — under HTTP/1.1 the 6-per-origin cap was
	// being eaten by (messages SSE) + (room SSE) + (signal SSE) + the
	// burst of ICE-candidate POSTs at cam-on time, which silently demoted
	// the room SSE and killed live chat / presence updates.
	sigInbox, sigUnsub := h.Bus.SubscribeSignal(roomID, id.Key())
	defer sigUnsub()

	h.Log.Info("rooms stream open", "room", roomID, "user", id.UserID, "guest", id.GuestID, "key", id.Key())
	defer h.Log.Info("rooms stream close", "room", roomID, "user", id.UserID, "guest", id.GuestID)

	// We need the public scheme+host to build copy-able share-link URLs
	// inside fragment pushes. Resolve once from this request.
	scheme, host := publicSchemeHost(r)

	h.pushRoomFragments(r.Context(), sse, rm, id, scheme, host)

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_ = sse.PatchSignals([]byte(`{}`))
		case ev := <-events:
			rm2, err := h.Repo.RoomByID(r.Context(), roomID)
			if err != nil {
				h.Log.Warn("rooms stream: load room failed", "err", err, "room", roomID, "key", id.Key())
				continue
			}
			h.Log.Info("rooms stream push", "kind", ev.Kind, "room", roomID, "key", id.Key())
			switch ev.Kind {
			case "presence", "approval":
				h.pushParticipants(r.Context(), sse, rm2.ID, id)
				// Promotion / admin transfer also flips the recipient's
				// admin status. Pushing the admin panel here ensures the
				// just-promoted user sees their full controls (invite link,
				// public toggle, rename) instead of stale non-admin content.
				h.pushAdminPanel(r.Context(), sse, rm2, id, scheme, host)
			case "chat":
				h.pushChat(r.Context(), sse, rm2.ID, id)
			case "meta":
				h.pushRoomFragments(r.Context(), sse, rm2, id, scheme, host)
			}
		case env, ok := <-sigInbox:
			if !ok {
				// Another GetStream for the same key just subscribed and
				// closed our mailbox out from under us. Without this log
				// it's invisible — the room SSE just stops with no error.
				h.Log.Info("rooms stream: sig mailbox closed, exiting",
					"room", roomID, "key", id.Key())
				return
			}
			h.pushSignal(sse, env)
		}
	}
}

func (h *Handler) pushRoomFragments(ctx context.Context, sse *datastar.ServerSentEventGenerator, rm Room, viewer Identity, scheme, host string) {
	h.pushParticipants(ctx, sse, rm.ID, viewer)
	h.pushChat(ctx, sse, rm.ID, viewer)
	h.pushAdminPanel(ctx, sse, rm, viewer, scheme, host)
	_ = sse.PatchSignals([]byte(`{"rooms_room_name":"` + jsQuote(rm.Name) + `"}`))
}

func (h *Handler) pushParticipants(ctx context.Context, sse *datastar.ServerSentEventGenerator, roomID string, viewer Identity) {
	snap := h.State.Snapshot(roomID)
	members := toParticipantViews(snap.Members, snap.AdminKey, viewer.Key())
	pending := toParticipantViews(snap.Pending, "", viewer.Key())
	isAdmin := snap.AdminKey == viewer.Key() && !viewer.IsGuest()
	_ = sse.PatchElementTempl(
		webtempl.RoomsParticipants(members, pending, isAdmin, roomID),
		datastar.WithModeOuter(),
	)
	_ = sse.PatchElementTempl(
		webtempl.RoomsPendingBanner(pending, isAdmin, roomID),
		datastar.WithModeOuter(),
	)
	// am_admin is consumed by data-show on the gear button so a just-
	// promoted user gets their admin controls without a page reload.
	// Force-close the tray for demoted users so they can't keep a stale
	// view open after losing the role.
	patch := `{"rooms_member_count":` + intStr(snap.MemberCount) +
		`,"rooms_am_admin":` + boolJSON(isAdmin)
	if !isAdmin {
		patch += `,"rooms_admin_open":false`
	}
	patch += `}`
	_ = sse.PatchSignals([]byte(patch))
}

// pushAdminPanel re-renders the admin tray fragment (only meaningful when
// the viewer is the room admin — non-admins don't render that block).
func (h *Handler) pushAdminPanel(ctx context.Context, sse *datastar.ServerSentEventGenerator, rm Room, viewer Identity, scheme, host string) {
	if !h.State.IsAdmin(rm.ID, viewer.Key()) || viewer.IsGuest() {
		return
	}
	inviteURL := ""
	if inv, err := h.Repo.ActiveInviteForRoom(ctx, rm.ID); err == nil {
		inviteURL = scheme + "://" + host + "/rooms/invite/" + inv.Token
	}
	_ = sse.PatchElementTempl(
		webtempl.RoomsAdminPanel(webtempl.RoomAdminPanelData{
			RoomID:    rm.ID,
			IsPublic:  rm.IsPublic,
			InviteURL: inviteURL,
		}),
		datastar.WithSelector("#rooms-admin-panel"),
		datastar.WithModeOuter(),
	)
}

// pushSignal forwards a routed WebRTC signaling envelope to the page over
// the same SSE stream that carries room events. The client registers a
// global window.__roomsSignal handler in rooms.js. Folding signaling into
// the room stream keeps us under Chrome's HTTP/1.1 6-connection cap, which
// was being eaten by separate /signal/stream + ICE-candidate POST bursts.
func (h *Handler) pushSignal(sse *datastar.ServerSentEventGenerator, env SignalEnvelope) {
	payload, err := json.Marshal(map[string]string{
		"kind":    env.Kind,
		"from":    env.FromKey,
		"payload": env.Payload,
	})
	if err != nil {
		return
	}
	_ = sse.ExecuteScript(
		"window.__roomsSignal && window.__roomsSignal(" + string(payload) + ")",
	)
}

func publicSchemeHost(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme, host
}

// jsQuote escapes a string for inclusion inside a JSON double-quoted value.
// Only the four characters that break JSON parsing get escaped — anything
// safe enough to render in HTML body text is safe here.
func jsQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteRune('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (h *Handler) pushChat(ctx context.Context, sse *datastar.ServerSentEventGenerator, roomID string, viewer Identity) {
	msgs, err := h.Repo.ListChat(ctx, roomID, 200)
	if err != nil {
		return
	}
	views := toChatViews(msgs, viewer.UserID, viewer.GuestID)
	// Outer-mode patch: datastar morphs by the element's own id
	// ("rooms-chat-msgs"). This is the pattern privatemsg/community chat
	// use; the [data-rooms-chat] wrapper stays put while its <ul> swaps.
	_ = sse.PatchElementTempl(
		webtempl.RoomsChatList(views),
		datastar.WithModeOuter(),
	)
	// Pin the scroll to the bottom so the newest message is always visible.
	_ = sse.ExecuteScript(
		`document.querySelector('[data-rooms-chat]')?.scrollTo({top: 1e9, behavior: 'smooth'})`,
	)
}

// GetSignalStream is the raw SSE relay (separate connection from the room
// stream so message ordering is preserved and JS uses native EventSource).
func (h *Handler) GetSignalStream(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if !h.State.IsMember(roomID, id.Key()) {
		http.Error(w, "not in room", http.StatusForbidden)
		return
	}
	h.Svc.streamSignal(w, r, roomID, id.Key())
}

func (h *Handler) PostSignal(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Svc.RouteSignal(roomID, id.Key(), raw); err != nil {
		h.Log.Warn("rooms signal route failed",
			"err", err, "room", roomID, "from", id.Key(),
			"is_member", h.State.IsMember(roomID, id.Key()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) PostJoin(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if id.IsGuest() {
		// Guests joined via /rooms/invite/{token}/join already; nothing to
		// do here.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := h.Svc.JoinAuth(r.Context(), roomID, id.UserID, id.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/rooms/" + roomID)
}

// PostPing is the heartbeat the client sends every 15s. Updates last-seen
// in state; the janitor evicts members who go silent for 45s.
func (h *Handler) PostPing(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	h.State.Touch(roomID, id.Key(), time.Now().UTC())
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) PostLeave(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	_ = h.Svc.Leave(r.Context(), roomID, id.Key())
	if id.IsGuest() {
		h.Sessions.Remove(r.Context(), sessKeyGuestID)
		h.Sessions.Remove(r.Context(), sessKeyGuestName)
		h.Sessions.Remove(r.Context(), sessKeyGuestRoomID)
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/rooms")
}

type targetSignals struct {
	Target  string `json:"rooms_target"`
	Body    string `json:"rooms_chat_body"`
	NewName string `json:"rooms_new_name"`
}

func (h *Handler) PostApprove(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, func(svc *Service, id Identity, in targetSignals) error {
		return svc.Approve(r.Context(), chi.URLParam(r, "id"), id.Key(), in.Target)
	})
}
func (h *Handler) PostDecline(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, func(svc *Service, id Identity, in targetSignals) error {
		return svc.Decline(r.Context(), chi.URLParam(r, "id"), id.Key(), in.Target)
	})
}
func (h *Handler) PostPromote(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, func(svc *Service, id Identity, in targetSignals) error {
		return svc.Promote(r.Context(), chi.URLParam(r, "id"), id.Key(), in.Target)
	})
}
func (h *Handler) PostTogglePublic(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, func(svc *Service, id Identity, in targetSignals) error {
		_, err := svc.TogglePublic(r.Context(), chi.URLParam(r, "id"), id.Key())
		return err
	})
}
func (h *Handler) PostRename(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, func(svc *Service, id Identity, in targetSignals) error {
		return svc.Rename(r.Context(), chi.URLParam(r, "id"), id.Key(), in.NewName)
	})
}

func (h *Handler) PostChat(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in targetSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		h.Log.Warn("rooms chat: bad signals", "err", err, "room", roomID, "user", id.UserID, "guest", id.GuestID)
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Self-heal: the janitor evicts idle members after staleAfter. If an
	// auth user was just evicted, transparently rejoin them and retry —
	// they're still on the room page, they just paused. Guests aren't
	// auto-readmitted because their invite cookie + identity verification
	// already happens at /rooms/invite/{token}/join time.
	_, err := h.Svc.PostChat(r.Context(), roomID, id, in.Body)
	if errors.Is(err, ErrNotMember) && id.UserID != "" {
		if _, jerr := h.Svc.JoinAuth(r.Context(), roomID, id.UserID, id.Name); jerr == nil {
			_, err = h.Svc.PostChat(r.Context(), roomID, id, in.Body)
		}
	}
	if err != nil {
		h.Log.Warn("rooms chat: send failed",
			"err", err, "room", roomID, "user", id.UserID, "guest", id.GuestID,
			"body_len", len(in.Body), "key", id.Key(),
			"is_member", h.State.IsMember(roomID, id.Key()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"rooms_chat_body":""}`))
}

func (h *Handler) PostCreateInvite(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if id.IsGuest() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := h.Svc.CreateInvite(r.Context(), roomID, id.Key(), id.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The CreateInvite service published a "meta" event; the room SSE
	// stream picks it up and re-renders the admin panel fragment in place.
	// No redirect — datastar keeps the page state intact.
	w.WriteHeader(http.StatusNoContent)
}

// PostShareToChat posts a "join room" announcement in the admin's current
// community chat. Public rooms only — sharing a private room would leak
// access. We bypass the markdown pipeline and insert pre-built HTML with
// an explicit target="_blank" anchor so the chat renders the URL as a
// clickable button-style link instead of plain text.
func (h *Handler) PostShareToChat(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	if h.ChatRepo == nil || h.ChatBus == nil {
		http.Error(w, "chat sharing not available", http.StatusServiceUnavailable)
		return
	}
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if id.IsGuest() || !h.State.IsAdmin(roomID, id.Key()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rm, err := h.Repo.RoomByID(r.Context(), roomID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !rm.IsPublic {
		http.Error(w, "only public rooms can be shared", http.StatusBadRequest)
		return
	}
	commID := ""
	if ai, ok := auth.FromContext(r.Context()); ok {
		commID = ai.Membership.CommunityID
	}
	if commID == "" {
		commID = auth.CurrentCommunityID(r.Context(), h.Sessions)
	}
	if commID == "" {
		http.Error(w, "no community to share to", http.StatusBadRequest)
		return
	}
	scheme, host := publicSchemeHost(r)
	link := scheme + "://" + host + "/rooms/" + rm.ID
	name := htmlEsc(rm.Name)
	href := htmlEsc(link)
	bodyHTML := `🎥 Room <strong>` + name + `</strong> is live — ` +
		`<a href="` + href + `" target="_blank" rel="noopener">Join the meeting</a>`
	bodyMD := "🎥 Room " + rm.Name + " is live — " + link
	aid := id.UserID
	msg := chat.Message{
		ID:           uuid.NewString(),
		CommunityID:  commID,
		AuthorID:     &aid,
		Kind:         chat.KindUser,
		BodyMarkdown: bodyMD,
		BodyHTML:     bodyHTML,
		CreatedAt:    time.Now(),
	}
	if err := h.ChatRepo.Insert(r.Context(), msg); err != nil {
		http.Error(w, "post failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.ChatBus.Broadcast()
	w.WriteHeader(http.StatusNoContent)
}

// htmlEsc escapes the four characters that break HTML attribute / text
// context. Used because we hand-build the chat message HTML.
func htmlEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (h *Handler) PostRevokeInvite(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if id.IsGuest() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	inv, err := h.Repo.ActiveInviteForRoom(r.Context(), roomID)
	if err != nil {
		http.Error(w, "no active invite", http.StatusBadRequest)
		return
	}
	if err := h.Svc.RevokeInvite(r.Context(), roomID, id.Key(), inv.Token); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) GetInviteLanding(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	inv, err := h.Repo.InviteByToken(r.Context(), token)
	if err != nil || !inv.Active(time.Now().UTC()) {
		http.Error(w, "invite link is no longer valid", http.StatusNotFound)
		return
	}
	rm, err := h.Repo.RoomByID(r.Context(), inv.RoomID)
	if err != nil {
		http.Error(w, "room missing", http.StatusNotFound)
		return
	}
	_ = webtempl.RoomGuestJoinPage(webtempl.GuestJoinPageData{
		RoomName: rm.Name,
		Token:    token,
	}).Render(r.Context(), w)
}

func (h *Handler) PostInviteJoin(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	rm, gid, err := h.Svc.JoinGuest(r.Context(), token, name)
	if err != nil {
		_ = webtempl.RoomGuestJoinPage(webtempl.GuestJoinPageData{
			RoomName: "Meeting",
			Token:    token,
			Error:    err.Error(),
		}).Render(r.Context(), w)
		return
	}
	h.Sessions.Put(r.Context(), sessKeyGuestID, gid.GuestID)
	h.Sessions.Put(r.Context(), sessKeyGuestName, gid.Name)
	h.Sessions.Put(r.Context(), sessKeyGuestRoomID, rm.ID)
	http.Redirect(w, r, "/rooms/"+rm.ID, http.StatusSeeOther)
}

// adminAction is the shared decoder + dispatcher for the admin-only POSTs
// that take a `rooms_target` signal.
func (h *Handler) adminAction(w http.ResponseWriter, r *http.Request, fn func(*Service, Identity, targetSignals) error) {
	roomID := chi.URLParam(r, "id")
	id, ok := h.caller(r, roomID)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if id.IsGuest() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in targetSignals
	if err := datastar.ReadSignals(r, &in); err != nil && err != io.EOF {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := fn(h.Svc, id, in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) layoutViewer(r *http.Request) webtempl.Viewer {
	uid := auth.CurrentUserID(r.Context(), h.Sessions)
	if uid == "" {
		return webtempl.Viewer{}
	}
	name := h.resolveAuthName(r.Context(), uid)
	v := webtempl.Viewer{IsAuthed: true, DisplayName: name}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.Role = string(id.Membership.Role)
	}
	return v
}

func toParticipantViews(ids []Identity, adminKey, myKey string) []webtempl.RoomParticipantView {
	out := make([]webtempl.RoomParticipantView, 0, len(ids))
	for _, id := range ids {
		out = append(out, webtempl.RoomParticipantView{
			Key:     id.Key(),
			Name:    id.Name,
			IsAdmin: id.Key() == adminKey,
			IsGuest: id.IsGuest(),
			IsMe:    id.Key() == myKey,
		})
	}
	return out
}

func toChatViews(msgs []ChatMessage, viewerUID, viewerGID string) []webtempl.RoomChatView {
	out := make([]webtempl.RoomChatView, 0, len(msgs))
	for _, m := range msgs {
		// IsMine: auth users compare on user-id. Guests can't claim ownership
		// because guest IDs aren't persisted on chat rows.
		_ = viewerGID
		mine := viewerUID != "" && m.AuthorUserID == viewerUID
		out = append(out, webtempl.RoomChatView{
			ID:         m.ID,
			AuthorName: m.AuthorName,
			BodyHTML:   m.BodyHTML,
			CreatedAt:  m.CreatedAt,
			IsMine:     mine,
			IsGuest:    m.AuthorUserID == "",
		})
	}
	return out
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func nowMillisJSON() string {
	return intStr(int(time.Now().UnixMilli()))
}
