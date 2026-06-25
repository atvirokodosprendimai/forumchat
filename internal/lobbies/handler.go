package lobbies

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

const (
	// RecentLimit caps the message history rendered on every page load
	// and SSE refresh. Matches chat's window.
	RecentLimit = 100

	// PasteImageMaxBytes mirrors chat's 1 MiB cap for inlined paste/drop
	// image data after base64 decoding.
	PasteImageMaxBytes = 1 << 20
)

// PushNotifier is the slice of push.Sender the host needs for guest
// notifications. Defining locally keeps the lobbies package free of a
// push dependency.
type PushNotifier func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string)

// Handler wires lobbies HTTP routes. Mounted twice in main: host-side
// routes inside the /c/{slug} admin/mod group, guest-side routes at
// the root path (token-authed).
type Handler struct {
	Svc           *Service
	Repo          *Repo
	Bus           *Bus
	NATS          *nats.Conn
	Uploads       *uploads.Store
	SessionSecret string
	PushNotify    PushNotifier
	Log           *slog.Logger
}

// ---- host-side helpers ----------------------------------------------------

func (h *Handler) cid(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.ID
	}
	return ""
}

func (h *Handler) cname(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.Name
	}
	return ""
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

func (h *Handler) broadcast(ctx context.Context, lobbyID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(lobbyID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.LobbySubject(h.cid(ctx), lobbyID), []byte("changed"))
	}
}

// ---- host-side handlers ---------------------------------------------------

// GetIndex renders the per-community list of lobbies, split into Open
// (default) and Archived tabs via ?status=archived.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = StatusOpen
	}
	rows, err := h.Repo.ListByCommunity(r.Context(), h.cid(r.Context()), status)
	if err != nil {
		http.Error(w, "load lobbies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.LobbiesIndex(webtempl.LobbiesIndexData{
		Viewer: h.viewer(r),
		Status: status,
		Rows:   rowsToView(rows),
	}).Render(r.Context(), w)
}

type mintSignals struct {
	Medium         string `json:"lobby_medium"`
	GuestName      string `json:"lobby_guest_name"`
	GuestEmail     string `json:"lobby_guest_email"`
	ExpiresInHours string `json:"lobby_expires_hours"`
}

// PostNew mints a lobby from the host form and patches the freshly-
// created row into the list + opens a copy-modal with the share URL.
func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in mintSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	medium := strings.TrimSpace(in.Medium)
	if medium == "" {
		medium = MediumLobby
	}
	var expires time.Duration
	if v := strings.TrimSpace(in.ExpiresInHours); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			expires = time.Duration(n) * time.Hour
		}
	}
	l, err := h.Svc.Mint(r.Context(), MintInput{
		CommunityID: h.cid(r.Context()),
		HostUserID:  id.User.ID,
		Medium:      medium,
		GuestName:   in.GuestName,
		GuestEmail:  in.GuestEmail,
		ExpiresIn:   expires,
	})
	if err != nil {
		http.Error(w, "mint: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	url := guestURL(r, l.GuestToken)
	_ = sse.PatchElementTempl(webtempl.LobbiesInviteCreated(h.cslug(r.Context()), lobbyToView(l), url))
	rows, err := h.Repo.ListByCommunity(r.Context(), h.cid(r.Context()), StatusOpen)
	if err == nil {
		_ = sse.PatchElementTempl(webtempl.LobbiesList(h.cslug(r.Context()), StatusOpen, rowsToView(rows)))
	}
}

// GetHostView renders the host-side chat UI for one lobby.
func (h *Handler) GetHostView(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	msgs, _ := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
	_ = webtempl.LobbyHostView(webtempl.LobbyHostViewData{
		Viewer:        h.viewer(r),
		Lobby:         lobbyToView(l),
		Messages:      messagesToView(msgs, id.Membership.DisplayName),
		GuestURL:      guestURL(r, l.GuestToken),
		CurrentUserID: id.User.ID,
	}).Render(r.Context(), w)
}

// GetHostStream subscribes to the lobby's bus + NATS subject and
// rerenders the message list on every event.
func (h *Handler) GetHostStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	h.streamMessages(w, r, l, id.Membership.DisplayName)
}

type hostSendSignals struct {
	Body      string `json:"lobby_body"`
	ImageData string `json:"lobby_image_data"`
}

// PostHostSend persists a host message, broadcasts the bus event, and
// clears the composer signals via PatchSignals.
func (h *Handler) PostHostSend(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in hostSendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := composedBody(r.Context(), h.Uploads, id.User.ID, l.CommunityID, in.Body, in.ImageData, h.Log)
	sse := render.NewSSE(w, r)
	if body == "" {
		return
	}
	hostID := id.User.ID
	if _, err := h.Svc.Send(r.Context(), SendInput{
		LobbyID:      l.ID,
		AuthorKind:   AuthorHost,
		AuthorUserID: &hostID,
		BodyMarkdown: body,
	}); err != nil {
		h.Log.Error("host send", "err", err)
		return
	}
	msgs, _ := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
	_ = sse.PatchElementTempl(webtempl.LobbyMessages(messagesToView(msgs, id.Membership.DisplayName)))
	_ = sse.PatchSignals([]byte(`{"lobby_body":"","lobby_image_data":""}`))
	h.broadcast(r.Context(), l.ID)
}

// PostClose marks the lobby closed: guest URL becomes 410, host
// retains full history.
func (h *Handler) PostClose(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, StatusClosed)
}

// PostArchive moves the lobby to the archived tab. URL keeps working.
func (h *Handler) PostArchive(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, StatusArchived)
}

// PostReopen moves an archived lobby back to open.
func (h *Handler) PostReopen(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, StatusOpen)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, status string) {
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if err := h.Repo.UpdateStatus(r.Context(), l.ID, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.broadcast(r.Context(), l.ID)
	sse := render.NewSSE(w, r)
	// Patch the host-side header so the open lobby page swaps buttons
	// (Close→Reopen etc.) and status badge in place. Harmless when the
	// host happens to be on the list page instead — datastar morph is
	// a no-op when #lobby-host-header isn't in the DOM.
	fresh, err := h.Repo.ByID(r.Context(), l.ID)
	if err == nil {
		_ = sse.PatchElementTempl(webtempl.LobbyHostHeader(h.cslug(r.Context()), lobbyToView(fresh), guestURL(r, fresh.GuestToken)))
	}
	listStatus := StatusOpen
	if status == StatusArchived {
		listStatus = StatusArchived
	} else if status == StatusClosed {
		listStatus = StatusClosed
	}
	rows, err := h.Repo.ListByCommunity(r.Context(), h.cid(r.Context()), listStatus)
	if err == nil {
		_ = sse.PatchElementTempl(webtempl.LobbiesList(h.cslug(r.Context()), listStatus, rowsToView(rows)))
	}
}

type updateGuestSignals struct {
	Name  string `json:"lobby_edit_name"`
	Email string `json:"lobby_edit_email"`
}

// PostUpdateGuest lets the host fix the captured display name / email
// after mint — needed when Promote requires an email that wasn't
// supplied originally, or when the guest typo'd their own name on
// join. Patches the header so the new fields show immediately.
func (h *Handler) PostUpdateGuest(w http.ResponseWriter, r *http.Request) {
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	var in updateGuestSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(in.Name)
	email := strings.TrimSpace(in.Email)
	if err := h.Repo.UpdateGuestProfile(r.Context(), l.ID, name, email); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fresh, err := h.Repo.ByID(r.Context(), l.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.LobbyHostHeader(h.cslug(r.Context()), lobbyToView(fresh), guestURL(r, fresh.GuestToken)))
}

// PostPromote issues an invite code bound to the lobby's stored guest
// email and renders the copy-able code into the host view.
func (h *Handler) PostPromote(w http.ResponseWriter, r *http.Request) {
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	code, err := h.Svc.Promote(r.Context(), l.ID)
	if err != nil {
		msg := "Promote failed."
		if errors.Is(err, ErrPromoteNeedsEmail) {
			msg = "Set a guest email first (Edit guest profile above)."
		}
		_ = sse.PatchElementTempl(webtempl.LobbyPromoteResult("", msg))
		return
	}
	_ = sse.PatchElementTempl(webtempl.LobbyPromoteResult(code, ""))
}

// PostDelete hard-deletes the lobby + cascades messages. Admin only.
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || !id.Membership.Role.AtLeast(auth.RoleAdmin) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	lobbyID := chi.URLParam(r, "id")
	l, err := h.Repo.ByID(r.Context(), lobbyID)
	if err != nil || l.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if err := h.Repo.Delete(r.Context(), l.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/lobbies")
}

// ---- guest-side handlers --------------------------------------------------

// GetGuestView serves the landing page (name capture) on first visit
// or the chat UI once the cookie is set.
func (h *Handler) GetGuestView(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	l, err := h.Repo.ByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !l.IsOpen(time.Now()) {
		http.Redirect(w, r, "/lobby/"+token+"/closed", http.StatusSeeOther)
		return
	}
	communityName, _ := h.communityName(r.Context(), l.CommunityID)
	hostName, _ := h.hostName(r.Context(), l.HostUserID, l.CommunityID)
	joinedLobbyID, joined := GuestLobbyFromRequest(r, h.SessionSecret)
	if !joined || joinedLobbyID != l.ID || strings.TrimSpace(l.GuestDisplayName) == "" {
		_ = webtempl.GuestLanding(webtempl.GuestLandingData{
			Token:         token,
			CommunityName: communityName,
			HostName:      hostName,
			PrefillName:   l.GuestDisplayName,
		}).Render(r.Context(), w)
		return
	}
	msgs, _ := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
	_ = webtempl.GuestChat(webtempl.GuestChatData{
		Token:         token,
		CommunityName: communityName,
		HostName:      hostName,
		GuestName:     l.GuestDisplayName,
		Messages:      messagesToView(msgs, l.GuestDisplayName),
	}).Render(r.Context(), w)
}

type guestJoinSignals struct {
	GuestName string `json:"lobby_guest_name"`
}

// PostGuestJoin stamps the chosen display name + sets the signed
// cookie so the next visit lands on the chat directly.
func (h *Handler) PostGuestJoin(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	var in guestJoinSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	l, err := h.Svc.Join(r.Context(), token, in.GuestName)
	if err != nil {
		if errors.Is(err, ErrClosedOrExpired) {
			http.Redirect(w, r, "/lobby/"+token+"/closed", http.StatusSeeOther)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	SetGuestCookie(w, l.ID, h.SessionSecret)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/lobby/" + token)
}

type guestSendSignals struct {
	Body      string `json:"lobby_body"`
	ImageData string `json:"lobby_image_data"`
}

// PostGuestSend persists a guest message. Authn = signed cookie + the
// cookie's lobby_id must match the token's lobby_id (defence against a
// stale cookie leaking across lobbies).
func (h *Handler) PostGuestSend(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	l, err := h.Repo.ByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cookieLobbyID, ok := GuestLobbyFromRequest(r, h.SessionSecret)
	if !ok || cookieLobbyID != l.ID {
		http.Error(w, "join required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in guestSendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	syntheticUserID := "lobby:" + l.ID
	body := composedBody(r.Context(), h.Uploads, syntheticUserID, l.CommunityID, in.Body, in.ImageData, h.Log)
	sse := render.NewSSE(w, r)
	if body == "" {
		return
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		LobbyID:      l.ID,
		AuthorKind:   AuthorGuest,
		BodyMarkdown: body,
	}); err != nil {
		if errors.Is(err, ErrClosedOrExpired) {
			http.Redirect(w, r, "/lobby/"+token+"/closed", http.StatusSeeOther)
			return
		}
		h.Log.Error("guest send", "err", err)
		return
	}
	msgs, _ := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
	_ = sse.PatchElementTempl(webtempl.LobbyMessages(messagesToView(msgs, l.GuestDisplayName)))
	_ = sse.PatchSignals([]byte(`{"lobby_body":"","lobby_image_data":""}`))
	h.broadcast(r.Context(), l.ID)

	// Fire-and-forget push notification for the host.
	if h.PushNotify != nil {
		cid := l.CommunityID
		hostID := l.HostUserID
		title := l.GuestDisplayName + " replied"
		preview := bodyPreview(body, 120)
		url := "/c/" + h.cslug(r.Context()) + "/lobbies/" + l.ID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h.PushNotify(ctx, cid, "lobby_message", []string{hostID}, title, preview, url)
		}()
	}
}

// GetGuestStream is the guest-side SSE handler.
func (h *Handler) GetGuestStream(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	l, err := h.Repo.ByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cookieLobbyID, ok := GuestLobbyFromRequest(r, h.SessionSecret)
	if !ok || cookieLobbyID != l.ID {
		http.Error(w, "join required", http.StatusUnauthorized)
		return
	}
	h.streamMessages(w, r, l, l.GuestDisplayName)
}

// GetClosed renders the read-only closed-lobby page for guests.
func (h *Handler) GetClosed(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	l, err := h.Repo.ByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cookieLobbyID, ok := GuestLobbyFromRequest(r, h.SessionSecret)
	hasHistory := ok && cookieLobbyID == l.ID
	var msgs []LobbyMessage
	if hasHistory {
		msgs, _ = h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
	}
	communityName, _ := h.communityName(r.Context(), l.CommunityID)
	w.WriteHeader(http.StatusOK)
	_ = webtempl.GuestClosed(webtempl.GuestClosedData{
		Token:         token,
		CommunityName: communityName,
		HasHistory:    hasHistory,
		Messages:      messagesToView(msgs, l.GuestDisplayName),
	}).Render(r.Context(), w)
}

// PostGuestUpload pipes an image into the existing uploads.Store with
// a synthetic `lobby:<id>` user id so the signed-URL pipeline works
// without an authenticated user account.
func (h *Handler) PostGuestUpload(w http.ResponseWriter, r *http.Request) {
	// v1 keeps inline base64 image_data signal (same path as chat
	// paste/drop), so this endpoint is reserved for future multipart
	// uploads. Return 501 so callers fail loud if they try too early.
	http.Error(w, "not implemented; use image_data signal for v1", http.StatusNotImplemented)
}

// ---- shared streaming helper ---------------------------------------------

func (h *Handler) streamMessages(w http.ResponseWriter, r *http.Request, l Lobby, viewerName string) {
	sse := render.NewSSE(w, r)
	// initial sync
	if msgs, err := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit); err == nil {
		_ = sse.PatchElementTempl(webtempl.LobbyMessages(messagesToView(msgs, viewerName)))
	}
	local, unsubscribe := h.Bus.Subscribe(l.ID)
	defer unsubscribe()
	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.LobbySubject(l.CommunityID, l.ID), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe (lobby)", "err", err)
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
		msgs, err := h.Repo.RecentMessages(r.Context(), l.ID, RecentLimit)
		if err != nil {
			continue
		}
		if err := sse.PatchElementTempl(webtempl.LobbyMessages(messagesToView(msgs, viewerName))); err != nil {
			return
		}
	}
}

// ---- helpers --------------------------------------------------------------

func (h *Handler) communityName(ctx context.Context, communityID string) (string, error) {
	if c, ok := community.FromContext(ctx); ok && c.ID == communityID {
		return c.Name, nil
	}
	// Guest-side handlers don't have community on context; do a quick lookup.
	row := h.Repo.DB.QueryRowContext(ctx,
		`SELECT name FROM communities WHERE id = ?`, communityID)
	var name string
	err := row.Scan(&name)
	return name, err
}

func (h *Handler) hostName(ctx context.Context, hostID, communityID string) (string, error) {
	row := h.Repo.DB.QueryRowContext(ctx,
		`SELECT effective_display_name FROM memberships WHERE user_id = ? AND community_id = ?`,
		hostID, communityID)
	var name string
	err := row.Scan(&name)
	return name, err
}

func composedBody(ctx context.Context, store *uploads.Store, userID, communityID, body, imageData string, log *slog.Logger) string {
	body = strings.TrimSpace(body)
	if imageData != "" && store != nil {
		u, err := store.SaveDataURL(ctx, userID, communityID, imageData, PasteImageMaxBytes)
		if err != nil {
			log.Warn("lobby paste image", "err", err)
		} else {
			url := store.SignedURL(u.ID, userID, 24*time.Hour)
			imgMD := "[![](" + url + ")](" + url + ")"
			if body == "" {
				body = imgMD
			} else {
				body = imgMD + "\n\n" + body
			}
		}
	}
	if len(body) > 4000 {
		return ""
	}
	return body
}

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

func guestURL(r *http.Request, token string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/lobby/" + token
}

// ---- view-model adapters --------------------------------------------------

func lobbyToView(l Lobby) webtempl.LobbyView {
	return webtempl.LobbyView{
		ID:             l.ID,
		Medium:         l.Medium,
		GuestName:      l.GuestDisplayName,
		GuestEmail:     l.GuestEmail,
		Status:         l.Status,
		CreatedAt:      l.CreatedAt,
		LastActivityAt: l.LastActivityAt,
		ExpiresAt:      l.ExpiresAt,
	}
}

func rowsToView(rows []LobbyRow) []webtempl.LobbyView {
	out := make([]webtempl.LobbyView, 0, len(rows))
	for _, r := range rows {
		v := lobbyToView(r.Lobby)
		v.MessageCount = r.MessageCount
		v.LastMessageAt = r.LastMessageAt
		v.LastAuthorKind = r.LastAuthorKind
		out = append(out, v)
	}
	return out
}

func messagesToView(msgs []LobbyMessage, viewerName string) []webtempl.LobbyMsgView {
	out := make([]webtempl.LobbyMsgView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, webtempl.LobbyMsgView{
			ID:         m.ID,
			AuthorKind: m.AuthorKind,
			BodyHTML:   m.BodyHTML,
			CreatedAt:  m.CreatedAt,
			Deleted:    m.IsDeleted(),
		})
	}
	_ = viewerName
	return out
}
