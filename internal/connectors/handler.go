package connectors

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
)

// Handler serves the public connector endpoints (stream + signed send/actions)
// and the per-community admin CRUD page. It mirrors webhooks.Handler's field set
// (chat service/repo, buses, NATS) plus the connector-specific seams declared as
// closures so this package stays decoupled from auth/uploads/presence.
type Handler struct {
	Repo      *Repo
	Svc       *Service
	Chat      *chat.Service
	ChatRepo  *chat.Repo
	Bus       *chat.Bus // chat fan-out (per-channel fat-morph)
	NewMsgBus *chat.Bus // cross-page "new message" ping
	NATS      *natsgo.Conn
	BaseURL   string
	MaxBytes  int64 // send/action body cap; <=0 → 64 KiB
	Log       *slog.Logger

	// ResolveAttachments turns a message's upload ids into fetchable event
	// attachments (shared-signed URL + metadata). nil → events omit attachments.
	ResolveAttachments func(ctx context.Context, uploadIDs []string) []EventAttachment

	// Presence, if set, marks the connector's member online while a stream is
	// attached and returns a cleanup run on disconnect. nil → no presence.
	Presence func(communityID, userID, nick string) (cleanup func())

	// Moderation seams. Each is nil unless wired in main.go; a nil seam means the
	// capability is unavailable (501) even when granted. They encapsulate their
	// own fan-out so this handler stays thin.
	DeleteMessage func(ctx context.Context, communityID, messageID, byUserID string) error
	BanMember     func(ctx context.Context, communityID, targetUserID string, hours int) error
	RenameChannel func(ctx context.Context, communityID, channelID, name string) error
}

// maxBytes returns the configured send/action body cap, defaulting to 64 KiB.
func (h *Handler) maxBytes() int64 {
	if h.MaxBytes > 0 {
		return h.MaxBytes
	}
	return 64 << 10
}

// authed loads the connector named in the URL and verifies the request body's
// HMAC signature. It is the gate every signed write endpoint (send + actions)
// shares: an unknown id is a 404 (anti-enumeration); a missing/bad X-Signature
// is a 401. On success it returns the connector and the raw body for the caller
// to parse. The capability check is the caller's (it's per-action).
func (h *Handler) authed(w http.ResponseWriter, r *http.Request) (Connector, []byte, bool) {
	conn, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return Connector{}, nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBytes()))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return Connector{}, nil, false
	}
	if !VerifyBody(conn.Secret, body, r.Header.Get("X-Signature")) {
		// Generic message — never reveal whether the id or the signature was the
		// problem beyond the status code.
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return Connector{}, nil, false
	}
	return conn, body, true
}

// requireCap enforces a granted capability and that the matching seam is wired.
// Returns false (and writes the response) when the connector may not perform it.
func (h *Handler) requireCap(w http.ResponseWriter, conn Connector, cap string, wired bool) bool {
	if !conn.Can(cap) {
		http.Error(w, "capability not granted", http.StatusForbidden)
		return false
	}
	if !wired {
		http.Error(w, "action unavailable", http.StatusNotImplemented)
		return false
	}
	return true
}

// ----- send ------------------------------------------------------------------

type sendReq struct {
	Channel string `json:"channel"` // channel slug; optional when the connector has exactly one
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to"` // optional parent message id
}

// PostSend posts a message into a channel AS THE CONNECTOR'S MEMBER — a normal
// kind='user' message (no badge). Signed (body HMAC), capability-gated (CapSend),
// allowlist-enforced. Mirrors chat's own send fan-out so open browser tabs
// fat-morph the new human bubble live.
func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	conn, body, ok := h.authed(w, r)
	if !ok {
		return
	}
	if !conn.Can(CapSend) {
		http.Error(w, "capability not granted", http.StatusForbidden)
		return
	}
	var in sendReq
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	ch, ok := h.resolveChannel(w, r, conn, in.Channel)
	if !ok {
		return
	}
	var replyTo *string
	if rt := strings.TrimSpace(in.ReplyTo); rt != "" {
		replyTo = &rt
	}
	msg, err := h.Chat.Send(r.Context(), chat.SendInput{
		CommunityID:  conn.CommunityID,
		ChannelID:    ch.ID,
		AuthorID:     conn.UserID,
		BodyMarkdown: in.Body,
		ReplyToID:    replyTo,
	})
	if err != nil {
		h.Log.Warn("connectors: send", "connector", conn.ID, "err", err)
		_ = h.Repo.Stamp(r.Context(), conn.ID, "error")
		http.Error(w, "send failed", http.StatusBadRequest)
		return
	}
	h.fanout(conn.CommunityID, ch.ID)
	_ = h.Repo.Stamp(r.Context(), conn.ID, "ok")
	writeJSON(w, http.StatusOK, map[string]string{"id": msg.ID, "channel": ch.Slug})
}

// ----- moderation actions ----------------------------------------------------

type deleteReq struct {
	MessageID string `json:"message_id"`
}

// PostDeleteMessage soft-deletes a chat message (CapDelete). The message is
// hidden from everyone (§6.3a). Best-effort fan-out is the seam's responsibility.
func (h *Handler) PostDeleteMessage(w http.ResponseWriter, r *http.Request) {
	conn, body, ok := h.authed(w, r)
	if !ok {
		return
	}
	if !h.requireCap(w, conn, CapDelete, h.DeleteMessage != nil) {
		return
	}
	var in deleteReq
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := h.DeleteMessage(r.Context(), conn.CommunityID, in.MessageID, conn.UserID); err != nil {
		h.Log.Warn("connectors: delete", "connector", conn.ID, "err", err)
		http.Error(w, "delete failed", http.StatusBadRequest)
		return
	}
	_ = h.Repo.Stamp(r.Context(), conn.ID, "ok")
	writeJSON(w, http.StatusOK, map[string]string{"deleted": in.MessageID})
}

type banReq struct {
	UserID string `json:"user_id"`
	Hours  int    `json:"hours"` // 0 = permanent
}

// PostBan bans a member by user id (CapBan). The seam refuses to ban an
// admin/owner and stamps banned_until; the ban takes effect on the target's next
// request (auth.Loader). `user_id` is the stable id surfaced in stream events.
func (h *Handler) PostBan(w http.ResponseWriter, r *http.Request) {
	conn, body, ok := h.authed(w, r)
	if !ok {
		return
	}
	if !h.requireCap(w, conn, CapBan, h.BanMember != nil) {
		return
	}
	var in banReq
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.UserID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.UserID == conn.UserID {
		http.Error(w, "cannot ban self", http.StatusBadRequest)
		return
	}
	if err := h.BanMember(r.Context(), conn.CommunityID, in.UserID, in.Hours); err != nil {
		h.Log.Warn("connectors: ban", "connector", conn.ID, "err", err)
		http.Error(w, "ban failed", http.StatusBadRequest)
		return
	}
	_ = h.Repo.Stamp(r.Context(), conn.ID, "ok")
	writeJSON(w, http.StatusOK, map[string]string{"banned": in.UserID})
}

type renameReq struct {
	Channel string `json:"channel"` // channel slug to rename
	Name    string `json:"name"`
}

// PostRename renames a channel (CapRename). The seam refuses the default
// #general (chat.Service.RenameChannel) and broadcasts the structural change.
func (h *Handler) PostRename(w http.ResponseWriter, r *http.Request) {
	conn, body, ok := h.authed(w, r)
	if !ok {
		return
	}
	if !h.requireCap(w, conn, CapRename, h.RenameChannel != nil) {
		return
	}
	var in renameReq
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ch, ok := h.resolveChannel(w, r, conn, in.Channel)
	if !ok {
		return
	}
	if err := h.RenameChannel(r.Context(), conn.CommunityID, ch.ID, in.Name); err != nil {
		h.Log.Warn("connectors: rename", "connector", conn.ID, "err", err)
		http.Error(w, "rename failed", http.StatusBadRequest)
		return
	}
	_ = h.Repo.Stamp(r.Context(), conn.ID, "ok")
	writeJSON(w, http.StatusOK, map[string]string{"renamed": ch.ID, "name": in.Name})
}

// ----- helpers ---------------------------------------------------------------

// resolveChannel maps a requested channel slug to a channel in the connector's
// community and enforces the allowlist. An empty slug defaults to the connector's
// sole subscribed channel (or errors when ambiguous). On any failure it writes
// the HTTP response and returns ok=false.
func (h *Handler) resolveChannel(w http.ResponseWriter, r *http.Request, conn Connector, slug string) (chat.Channel, bool) {
	allowed, err := h.Repo.Channels(r.Context(), conn.ID)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return chat.Channel{}, false
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		// Default to the sole subscribed channel; ambiguous otherwise.
		if len(allowed) != 1 {
			http.Error(w, "channel required", http.StatusBadRequest)
			return chat.Channel{}, false
		}
		ch, err := h.ChatRepo.ChannelByID(r.Context(), allowed[0])
		if err != nil {
			http.Error(w, "unknown channel", http.StatusBadRequest)
			return chat.Channel{}, false
		}
		return ch, true
	}
	ch, err := h.ChatRepo.ChannelBySlug(r.Context(), conn.CommunityID, slug)
	if err != nil {
		http.Error(w, "unknown channel", http.StatusNotFound)
		return chat.Channel{}, false
	}
	// Allowlist: empty = all channels; otherwise the channel must be listed.
	if len(allowed) > 0 && !contains(allowed, ch.ID) {
		http.Error(w, "channel not allowed", http.StatusForbidden)
		return chat.Channel{}, false
	}
	return ch, true
}

// fanout refreshes open chat tabs (Bus + NATS) for a channel, exactly like the
// chat send path and the webhooks bridge (§6.3a / §6.8).
func (h *Handler) fanout(communityID, channelID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(channelID)
	}
	if h.NewMsgBus != nil {
		h.NewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte(channelID))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
