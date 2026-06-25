package connectors

import (
	"context"
	"encoding/json"
	"errors"
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
	// attached and returns a cleanup run on disconnect. The wiring (main.go) is
	// responsible for KEEPING the member fresh for the life of the stream — the
	// roster is TTL-based, so a one-shot touch would let a long-lived connector
	// fall back to "offline"; the seam re-touches on a heartbeat under the TTL.
	// nil → no presence (the member shows offline even while streaming).
	Presence func(communityID, userID, nick, avatar string) (cleanup func())

	// Moderation seams. Each is nil unless wired in main.go; a nil seam means the
	// capability is unavailable (501) even when granted. They encapsulate their
	// own fan-out so this handler stays thin.
	//
	// The first three reach domains this package already imports (chat) or must
	// NOT import (auth). The cross-domain ones below (forum / bookmarks / todos /
	// privatemsg) follow the same closure-seam rule — connectors imports none of
	// them; main.go wires the concrete calls. Channel-only actions (forward,
	// create/topic/archive/delete channel) call chat.Service directly (already a
	// field) instead of a seam, since no cross-package import is involved.
	DeleteMessage func(ctx context.Context, communityID, messageID, byUserID string) error
	BanMember     func(ctx context.Context, communityID, targetUserID string, hours int) error
	RenameChannel func(ctx context.Context, communityID, channelID, name string) error

	// PromoteToThread turns a chat message into a forum thread (CapPromote),
	// authored by the connector's member, and returns the new thread id.
	PromoteToThread func(ctx context.Context, communityID, messageID, byUserID string) (threadID string, err error)
	// AddBookmark saves a chat message to the connector member's bookmarks
	// (CapBookmark). note is optional.
	AddBookmark func(ctx context.Context, communityID, userID, messageID, note string) error
	// AddTodo adds a chat message to the connector member's to-dos (CapTodo) and
	// returns the new to-do id. title/note are optional (title defaults server-side).
	AddTodo func(ctx context.Context, communityID, userID, messageID, title, note string) (todoID string, err error)
	// SendDM opens or appends a direct-message thread from the connector member to
	// another member (CapDM). The seam rejects a recipient outside the community.
	SendDM func(ctx context.Context, communityID, fromUserID, toUserID, body string) (threadID string, err error)
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

// actionErr is an error that carries the HTTP status a signed-action closure
// wants returned, so the shared do() pipeline can map a validation/permission
// failure to the right code without each closure touching the ResponseWriter. A
// plain (non-actionErr) error from a closure is treated as a 400 "action failed"
// and logged + stamped as an error (an unexpected server-side failure).
type actionErr struct {
	code int
	msg  string
}

func (e actionErr) Error() string { return e.msg }

// errBad / errForbidden / errNotFound build the status-carrying errors a closure
// returns. errNotFound is deliberately generic (anti-enumeration: an unknown,
// cross-tenant, or already-deleted target must look identical to a 404).
func errBad(msg string) actionErr       { return actionErr{http.StatusBadRequest, msg} }
func errForbidden(msg string) actionErr { return actionErr{http.StatusForbidden, msg} }
func errNotFound() actionErr            { return actionErr{http.StatusNotFound, "not found"} }

// do is the one pipeline every signed write endpoint shares: authenticate (load
// connector + verify the body HMAC), enforce the granted capability + that the
// seam is wired, then run fn. fn does the action-specific parse + target
// resolution + work and returns the JSON payload to write on success, or an
// error (an actionErr for an expected client failure, any other error for an
// unexpected one). Centralising this means a new action is just a capability,
// a route, and a closure — never another copy of the auth/stamp/respond dance.
func (h *Handler) do(w http.ResponseWriter, r *http.Request, cap string, wired bool, fn func(ctx context.Context, conn Connector, body []byte) (any, error)) {
	conn, body, ok := h.authed(w, r)
	if !ok {
		return
	}
	if !h.requireCap(w, conn, cap, wired) {
		return
	}
	out, err := fn(r.Context(), conn, body)
	if err != nil {
		var ae actionErr
		if errors.As(err, &ae) {
			// Expected client-side failure (bad input, not allowed, unknown target).
			http.Error(w, ae.msg, ae.code)
			return
		}
		// Unexpected: a seam/service error. Log it, stamp the connector unhealthy,
		// and surface a generic 400 (the detail is in the logs, not on the wire).
		h.Log.Warn("connectors: "+cap, "connector", conn.ID, "err", err)
		_ = h.Repo.Stamp(r.Context(), conn.ID, "error")
		http.Error(w, "action failed", http.StatusBadRequest)
		return
	}
	_ = h.Repo.Stamp(r.Context(), conn.ID, "ok")
	writeJSON(w, http.StatusOK, out)
}

// Every endpoint below is a thin closure over do(): parse → resolve target
// (resolveChannel / resolveMessage enforce the allowlist) → act → return the
// JSON payload. do() owns auth, the capability+wired gate, error→status mapping,
// the health stamp, and the response.

// ----- send / messaging -------------------------------------------------------

type sendReq struct {
	Channel string `json:"channel"` // channel slug; optional when the connector has exactly one
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to"` // optional parent message id
}

// PostSend posts a message into a channel AS THE CONNECTOR'S MEMBER — a normal
// kind='user' message (no badge). Capability-gated (CapSend), allowlist-enforced
// via resolveChannel. Mirrors chat's own send fan-out so open tabs fat-morph the
// new human bubble live.
func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapSend, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in sendReq
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, errBad("bad json")
		}
		if strings.TrimSpace(in.Body) == "" {
			return nil, errBad("empty body")
		}
		ch, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		var replyTo *string
		if rt := strings.TrimSpace(in.ReplyTo); rt != "" {
			// The parent must be a live message in the SAME channel, so a connector
			// can't nest under (and surface a quote from) a message elsewhere.
			parent, perr := h.ChatRepo.ByID(ctx, rt)
			if perr != nil || parent.ChannelID != ch.ID {
				return nil, errBad("invalid reply_to")
			}
			replyTo = &rt
		}
		msg, err := h.Chat.Send(ctx, chat.SendInput{
			CommunityID:  conn.CommunityID,
			ChannelID:    ch.ID,
			AuthorID:     conn.UserID,
			BodyMarkdown: in.Body,
			ReplyToID:    replyTo,
		})
		if err != nil {
			return nil, err
		}
		h.fanout(conn.CommunityID, ch.ID)
		return map[string]string{"id": msg.ID, "channel": ch.Slug}, nil
	})
}

type forwardReq struct {
	MessageID string `json:"message_id"` // source message to forward
	Channel   string `json:"channel"`    // target channel slug
	Note      string `json:"note"`       // optional forwarder note (body)
}

// PostForward forwards a message into another of the connector's channels
// (CapForward) as its member, carrying the "Forwarded from #x" embed. The source
// (resolveMessage) and the target (resolveChannel) are each allowlist-checked, so
// a scoped connector can't forward out of or into a channel it can't see.
func (h *Handler) PostForward(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapForward, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in forwardReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
			return nil, errBad("bad json")
		}
		src, err := h.resolveMessage(ctx, conn, in.MessageID)
		if err != nil {
			return nil, err
		}
		target, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		msg, err := h.Chat.Forward(ctx, chat.ForwardInput{
			CommunityID:     conn.CommunityID,
			TargetChannelID: target.ID,
			AuthorID:        conn.UserID,
			Note:            in.Note,
			SourceMsgID:     src.ID,
		})
		if err != nil {
			return nil, err
		}
		h.fanout(conn.CommunityID, target.ID)
		return map[string]string{"id": msg.ID, "channel": target.Slug}, nil
	})
}

type messageRef struct {
	MessageID string `json:"message_id"`
}

// PostPromote turns a chat message into a forum thread (CapPromote) authored by
// the connector's member, via the forum bridge seam. Allowlist-checked on the
// source message.
func (h *Handler) PostPromote(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapPromote, h.PromoteToThread != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in messageRef
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
			return nil, errBad("bad json")
		}
		m, err := h.resolveMessage(ctx, conn, in.MessageID)
		if err != nil {
			return nil, err
		}
		threadID, err := h.PromoteToThread(ctx, conn.CommunityID, m.ID, conn.UserID)
		if err != nil {
			return nil, err
		}
		return map[string]string{"thread_id": threadID}, nil
	})
}

// ----- moderation: members ----------------------------------------------------

type banReq struct {
	UserID string `json:"user_id"`
	Hours  int    `json:"hours"` // 0 = permanent
}

// PostBan bans a member by user id (CapBan). The seam refuses to ban an
// admin/owner and stamps banned_until; the ban takes effect on the target's next
// request (auth.Loader). `user_id` is the stable id surfaced in stream events.
func (h *Handler) PostBan(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapBan, h.BanMember != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in banReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.UserID) == "" {
			return nil, errBad("bad json")
		}
		if in.UserID == conn.UserID {
			return nil, errBad("cannot ban self")
		}
		if err := h.BanMember(ctx, conn.CommunityID, in.UserID, in.Hours); err != nil {
			return nil, err
		}
		return map[string]string{"banned": in.UserID}, nil
	})
}

// ----- moderation: messages ---------------------------------------------------

// PostDeleteMessage soft-deletes a chat message (CapDelete). The message is
// hidden from everyone (§6.3a). The seam owns the fan-out. The target is
// allowlist-checked (resolveMessage).
func (h *Handler) PostDeleteMessage(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapDelete, h.DeleteMessage != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in messageRef
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
			return nil, errBad("bad json")
		}
		m, err := h.resolveMessage(ctx, conn, in.MessageID)
		if err != nil {
			return nil, err
		}
		if err := h.DeleteMessage(ctx, conn.CommunityID, m.ID, conn.UserID); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": m.ID}, nil
	})
}

// ----- channels ---------------------------------------------------------------

type renameReq struct {
	Channel string `json:"channel"` // channel slug to rename
	Name    string `json:"name"`
}

// PostRename renames a channel (CapRename). The seam refuses the default
// #general (chat.Service.RenameChannel) and broadcasts the structural change.
func (h *Handler) PostRename(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapRename, h.RenameChannel != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in renameReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Name) == "" {
			return nil, errBad("bad json")
		}
		ch, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		if err := h.RenameChannel(ctx, conn.CommunityID, ch.ID, in.Name); err != nil {
			return nil, err
		}
		return map[string]string{"renamed": ch.ID, "name": in.Name}, nil
	})
}

type createChannelReq struct {
	Name  string `json:"name"`
	Topic string `json:"topic"` // optional
}

// PostCreateChannel creates a new public channel (CapCreateChannel) owned by the
// connector's member. chat.Service.CreateChannel enforces the per-community cap,
// the reserved-slug rule, and uniqueness; we just broadcast the structural change.
// No allowlist check — a new channel can't be in any list yet (community power).
func (h *Handler) PostCreateChannel(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapCreateChannel, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in createChannelReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Name) == "" {
			return nil, errBad("bad json")
		}
		ch, err := h.Chat.CreateChannel(ctx, conn.CommunityID, conn.UserID, in.Name, in.Topic)
		if err != nil {
			return nil, err
		}
		h.structuralFanout(conn.CommunityID)
		return map[string]string{"id": ch.ID, "slug": ch.Slug}, nil
	})
}

type setTopicReq struct {
	Channel string `json:"channel"`
	Topic   string `json:"topic"`
}

// PostSetTopic sets a channel's topic line (CapSetTopic). Allowlist-enforced.
func (h *Handler) PostSetTopic(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapSetTopic, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in setTopicReq
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, errBad("bad json")
		}
		ch, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		if err := h.Chat.SetTopic(ctx, conn.CommunityID, ch.ID, in.Topic); err != nil {
			return nil, err
		}
		h.structuralFanout(conn.CommunityID)
		return map[string]string{"channel": ch.Slug, "topic": in.Topic}, nil
	})
}

type channelRef struct {
	Channel string `json:"channel"`
}

// PostArchiveChannel archives a channel (CapArchive) — it drops out of the
// switcher but its history survives. Allowlist-enforced.
func (h *Handler) PostArchiveChannel(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapArchive, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in channelRef
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, errBad("bad json")
		}
		ch, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		if err := h.Chat.Archive(ctx, conn.CommunityID, ch.ID); err != nil {
			return nil, err
		}
		h.structuralFanout(conn.CommunityID)
		return map[string]string{"archived": ch.Slug}, nil
	})
}

// PostDeleteChannel permanently deletes a channel and its messages
// (CapDeleteChannel) — destructive. chat.Service.Delete protects #general.
// Allowlist-enforced.
func (h *Handler) PostDeleteChannel(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapDeleteChannel, h.Chat != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in channelRef
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, errBad("bad json")
		}
		ch, err := h.resolveChannel(ctx, conn, in.Channel)
		if err != nil {
			return nil, err
		}
		if err := h.Chat.Delete(ctx, conn.CommunityID, ch.ID); err != nil {
			return nil, err
		}
		h.structuralFanout(conn.CommunityID)
		return map[string]string{"deleted": ch.Slug}, nil
	})
}

// ----- personal (the connector member's own state) ----------------------------

type bookmarkReq struct {
	MessageID string `json:"message_id"`
	Note      string `json:"note"`
}

// PostBookmark saves a message to the connector member's bookmarks (CapBookmark),
// via the seam. Allowlist-enforced on the target.
func (h *Handler) PostBookmark(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapBookmark, h.AddBookmark != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in bookmarkReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
			return nil, errBad("bad json")
		}
		m, err := h.resolveMessage(ctx, conn, in.MessageID)
		if err != nil {
			return nil, err
		}
		if err := h.AddBookmark(ctx, conn.CommunityID, conn.UserID, m.ID, in.Note); err != nil {
			return nil, err
		}
		return map[string]string{"bookmarked": m.ID}, nil
	})
}

type todoReq struct {
	MessageID string `json:"message_id"`
	Title     string `json:"title"`
	Note      string `json:"note"`
}

// PostTodo adds a message to the connector member's to-do list (CapTodo), via the
// seam. Allowlist-enforced on the target.
func (h *Handler) PostTodo(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapTodo, h.AddTodo != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in todoReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.MessageID) == "" {
			return nil, errBad("bad json")
		}
		m, err := h.resolveMessage(ctx, conn, in.MessageID)
		if err != nil {
			return nil, err
		}
		todoID, err := h.AddTodo(ctx, conn.CommunityID, conn.UserID, m.ID, in.Title, in.Note)
		if err != nil {
			return nil, err
		}
		return map[string]string{"todo_id": todoID}, nil
	})
}

type dmReq struct {
	UserID string `json:"user_id"`
	Body   string `json:"body"`
}

// PostDM opens or appends a direct-message thread from the connector's member to
// another member (CapDM), via the seam (which rejects a recipient outside the
// community and a blocked sender). No channel target, so no allowlist check.
func (h *Handler) PostDM(w http.ResponseWriter, r *http.Request) {
	h.do(w, r, CapDM, h.SendDM != nil, func(ctx context.Context, conn Connector, body []byte) (any, error) {
		var in dmReq
		if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.UserID) == "" {
			return nil, errBad("bad json")
		}
		if strings.TrimSpace(in.Body) == "" {
			return nil, errBad("empty body")
		}
		if in.UserID == conn.UserID {
			return nil, errBad("cannot message self")
		}
		threadID, err := h.SendDM(ctx, conn.CommunityID, conn.UserID, in.UserID, in.Body)
		if err != nil {
			return nil, err
		}
		return map[string]string{"thread_id": threadID}, nil
	})
}

// ----- helpers ---------------------------------------------------------------

// resolveChannel maps a requested channel slug to a channel in the connector's
// community and enforces the allowlist. An empty slug defaults to the connector's
// sole subscribed channel (or errors when ambiguous). It returns an actionErr on
// any expected failure (do() maps it to a status) or a plain error for an
// unexpected lookup failure.
func (h *Handler) resolveChannel(ctx context.Context, conn Connector, slug string) (chat.Channel, error) {
	allowed, err := h.Repo.Channels(ctx, conn.ID)
	if err != nil {
		return chat.Channel{}, err // unexpected → do() logs + 400
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		// Default to the sole subscribed channel; ambiguous otherwise.
		if len(allowed) != 1 {
			return chat.Channel{}, errBad("channel required")
		}
		ch, err := h.ChatRepo.ChannelByID(ctx, allowed[0])
		if err != nil || ch.IsArchived() {
			return chat.Channel{}, errBad("unknown channel")
		}
		return ch, nil
	}
	ch, err := h.ChatRepo.ChannelBySlug(ctx, conn.CommunityID, slug)
	if err != nil {
		return chat.Channel{}, errNotFound()
	}
	if ch.IsArchived() {
		return chat.Channel{}, errForbidden("channel archived")
	}
	// Allowlist: empty = all channels; otherwise the channel must be listed.
	if len(allowed) > 0 && !contains(allowed, ch.ID) {
		return chat.Channel{}, errForbidden("channel not allowed")
	}
	return ch, nil
}

// resolveMessage loads a live message the connector may act on by id: it must
// exist (ByID never returns a soft-deleted row), be in the connector's community,
// and live in an allowed channel. A connector scoped to channel X must not reach
// a message in channel Y just because it knows the id. Returns errNotFound for an
// unknown / cross-tenant / already-deleted target (no existence oracle) and
// errForbidden when the channel is outside the allowlist.
func (h *Handler) resolveMessage(ctx context.Context, conn Connector, messageID string) (chat.Message, error) {
	m, err := h.ChatRepo.ByID(ctx, messageID)
	if err != nil || m.CommunityID != conn.CommunityID {
		return chat.Message{}, errNotFound()
	}
	if !h.channelAllowed(ctx, conn, m.ChannelID) {
		return chat.Message{}, errForbidden("channel not allowed")
	}
	return m, nil
}

// channelAllowed reports whether a connector may act on a given channel id: an
// empty allowlist means all channels, otherwise the channel must be listed. Used
// by moderation actions that target a message/channel by id (not slug), so the
// allowlist is enforced on the resolved target, not just on send.
func (h *Handler) channelAllowed(ctx context.Context, conn Connector, channelID string) bool {
	allowed, err := h.Repo.Channels(ctx, conn.ID)
	if err != nil {
		return false
	}
	return len(allowed) == 0 || contains(allowed, channelID)
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

// structuralFanout signals a structural chat change (channel created / renamed /
// topic / archived / deleted) to open tabs: an empty channel id on the chat Bus +
// NATS makes the channel switchers re-render (§6.8). Mirrors the rename seam's
// broadcast, used by the channel-management actions that call chat.Service directly.
func (h *Handler) structuralFanout(communityID string) {
	if h.Bus != nil {
		h.Bus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte(""))
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
