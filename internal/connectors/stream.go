package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
)

const (
	// streamBatchLimit caps how many messages one channel event loads at once.
	streamBatchLimit = 200
	// heartbeatEvery keeps an idle stream from being reaped by proxies.
	heartbeatEvery = 25 * time.Second
)

// readyEvent is the one-shot handshake frame: it tells the worker who it is and
// which channels it will receive, so a stateless worker can configure itself
// from the stream alone.
type readyEvent struct {
	Connector string         `json:"connector"`
	Nick      string         `json:"nick"`
	Channels  []channelBrief `json:"channels"`
}

type channelBrief struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// GetStream is the long-lived, signed JSON SSE read stream. The signed URL is
// the bearer credential (an EventSource can't set headers); the wire is raw
// text/event-stream JSON, NOT datastar (the consumer is a machine). It delivers
// every new message in the connector's subscribed channels, skipping the
// connector's own posts (no echo), system/structural rows, and — when
// mentions_only — anything that doesn't @mention the connector.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	conn, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if !VerifyStream(conn.Secret, conn.ID, r.URL.Query().Get("sig"), exp, time.Now()) {
		// Bad/missing signature: look like a non-existent URL (no oracle).
		http.NotFound(w, r)
		return
	}

	// Resolve the subscribed channel set (empty allowlist = all non-archived).
	subs, err := h.subscribedChannels(r.Context(), conn)
	if err != nil {
		http.Error(w, "load channels: "+err.Error(), http.StatusInternalServerError)
		return
	}

	flush, ok := h.openEventStream(w)
	if !ok {
		return
	}

	// Presence: show the member online while the worker is attached.
	if h.Presence != nil {
		if cleanup := h.Presence(conn.CommunityID, conn.UserID, conn.Name); cleanup != nil {
			defer cleanup()
		}
	}
	_ = h.Repo.Stamp(r.Context(), conn.ID, "connected")

	// Handshake.
	briefs := make([]channelBrief, 0, len(subs))
	for _, ch := range subs {
		briefs = append(briefs, channelBrief{ID: ch.ID, Slug: ch.Slug, Name: ch.Name})
	}
	if !writeFrame(w, flush, "ready", readyEvent{Connector: conn.ID, Nick: conn.Name, Channels: briefs}) {
		return
	}

	// Per-channel watermark (inclusive) + a seen-set for the boundary second so
	// same-second messages (created_at is unix seconds) are neither missed nor
	// duplicated. Seed seen with anything already at the connect second so we
	// stay live-only (no pre-connect backlog).
	now := time.Now()
	wm := make(map[string]time.Time, len(subs))
	seen := make(map[string]map[string]bool, len(subs))
	for id := range subs {
		wm[id] = now
		seen[id] = map[string]bool{}
		if existing, err := h.ChatRepo.ListAfter(r.Context(), id, now, streamBatchLimit); err == nil {
			for _, m := range existing {
				seen[id][m.ID] = true
			}
		}
	}

	local, unsubscribe := h.Bus.Subscribe()
	defer unsubscribe()

	var natsCh chan *natsgo.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *natsgo.Msg, 32)
		if sub, err := h.NATS.ChanSubscribe(natsx.ChatSubject(conn.CommunityID), natsCh); err == nil {
			defer sub.Unsubscribe()
		} else {
			natsCh = nil
		}
	}

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()

	for {
		var changed string
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// SSE comment heartbeat — keeps proxies from reaping the connection.
			if _, err := w.Write([]byte(":\n\n")); err != nil {
				return
			}
			flush()
			continue
		case changed = <-local:
		case msg, ok := <-natsCh:
			if !ok {
				natsCh = nil
				continue
			}
			changed = string(msg.Data)
		}
		// "" = a structural change (channel CRUD / system message). The connector
		// stream is message-oriented, so we re-check every subscribed channel for
		// new content; a specific id only re-checks that channel.
		if changed == "" {
			for id := range subs {
				if !h.drainChannel(r.Context(), w, flush, conn, subs, wm, seen, id) {
					return
				}
			}
			continue
		}
		if _, ok := subs[changed]; !ok {
			continue // a channel this connector doesn't subscribe to
		}
		if !h.drainChannel(r.Context(), w, flush, conn, subs, wm, seen, changed) {
			return
		}
	}
}

// drainChannel emits every new (unseen, deliverable) message in one channel and
// advances that channel's watermark + seen-set. Returns false if the client
// connection broke (caller should return).
func (h *Handler) drainChannel(ctx context.Context, w http.ResponseWriter, flush func(), conn Connector,
	subs map[string]chat.Channel, wm map[string]time.Time, seen map[string]map[string]bool, channelID string) bool {

	msgs, err := h.ChatRepo.ListAfter(ctx, channelID, wm[channelID], streamBatchLimit)
	if err != nil {
		h.Log.Warn("connectors: stream load", "connector", conn.ID, "channel", channelID, "err", err)
		return true // transient; keep the stream open
	}
	ch := subs[channelID]
	maxTS := wm[channelID]
	for _, m := range msgs {
		if m.CreatedAt.After(maxTS) {
			maxTS = m.CreatedAt
		}
		if seen[channelID][m.ID] {
			continue // already delivered (boundary-second dedupe)
		}
		if !h.deliver(ctx, w, flush, conn, ch, m) {
			return false
		}
	}
	// Advance the watermark and rebuild the seen-set to exactly the ids at the
	// new boundary second, so the next inclusive query skips them but still
	// catches a fresh same-second arrival.
	wm[channelID] = maxTS
	next := map[string]bool{}
	for _, m := range msgs {
		if m.CreatedAt.Equal(maxTS) {
			next[m.ID] = true
		}
	}
	seen[channelID] = next
	return true
}

// deliver decides whether a message is streamed to this connector and, if so,
// writes the `message` frame. It records the id in the seen-set regardless (so a
// filtered message isn't reconsidered). Returns false on a broken connection.
func (h *Handler) deliver(ctx context.Context, w http.ResponseWriter, flush func(), conn Connector, ch chat.Channel, m chat.Message) bool {
	// Filter: never echo the connector's own posts; skip system/structural rows.
	if m.AuthorID != nil && *m.AuthorID == conn.UserID {
		return true
	}
	if !deliverableKind(m.Kind) {
		return true
	}
	mentioned := Mentions(m.BodyMarkdown, conn.Name)
	if conn.MentionsOnly && !mentioned {
		return true
	}
	var atts []EventAttachment
	if h.ResolveAttachments != nil && len(m.Attachments) > 0 {
		ids := make([]string, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			ids = append(ids, a.UploadID)
		}
		atts = h.ResolveAttachments(ctx, ids)
	}
	return writeFrame(w, flush, "message", toEvent(m, ch.Slug, ch.Name, mentioned, atts))
}

// deliverableKind reports whether a message kind is streamed to a connector.
// Human (user), inbound bot (webhook) and chat-agent (bot) content is delivered;
// system + thread-announce rows are structural noise and skipped.
func deliverableKind(k chat.Kind) bool {
	switch k {
	case chat.KindUser, chat.KindWebhook, chat.KindBot:
		return true
	default:
		return false
	}
}

// subscribedChannels resolves the connector's channel set: its allowlist
// intersected with the community's non-archived channels, or ALL non-archived
// channels when the allowlist is empty.
func (h *Handler) subscribedChannels(ctx context.Context, conn Connector) (map[string]chat.Channel, error) {
	channels, err := h.ChatRepo.ListChannels(ctx, conn.CommunityID, false)
	if err != nil {
		return nil, err
	}
	allowed, err := h.Repo.Channels(ctx, conn.ID)
	if err != nil {
		return nil, err
	}
	allowSet := map[string]bool{}
	for _, id := range allowed {
		allowSet[id] = true
	}
	out := make(map[string]chat.Channel, len(channels))
	for _, ch := range channels {
		if len(allowed) == 0 || allowSet[ch.ID] {
			out[ch.ID] = ch
		}
	}
	return out, nil
}

// openEventStream sets the SSE headers, commits a 200, and returns a flush func.
// Raw text/event-stream — not datastar. Headers are set BEFORE WriteHeader so
// they reach the client; the flusher (http.ResponseController) walks the writer
// chain to the underlying http.Flusher.
func (h *Handler) openEventStream(w http.ResponseWriter) (func(), bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	flush := func() { _ = rc.Flush() }
	flush()
	return flush, true
}

// writeFrame writes one SSE event (`event: <name>\ndata: <json>\n\n`) and
// flushes. Returns false if the write failed (client gone).
func writeFrame(w http.ResponseWriter, flush func(), event string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return true // skip an unencodable frame rather than kill the stream
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return false
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return false
	}
	flush()
	return true
}
