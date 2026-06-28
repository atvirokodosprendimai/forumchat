package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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
	// maxCatchupWindow bounds how far back a reconnecting worker may replay. The
	// signed stream URL is a bearer capability on a public route, so an unbounded
	// resume (?since=0, or a days-old server cursor) would let one connect replay
	// all history; the clamp caps cost and memory. 24h mirrors the upload-orphan
	// sweep window (§6.7) and the chat-replay roadmap horizon. Anything older is
	// truncated (the `live` marker reports it).
	maxCatchupWindow = 24 * time.Hour
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

// liveEvent is the one-shot boundary frame emitted after any backlog replay,
// telling the worker "history (if any) is flushed — everything from here is
// live". It makes the contract uniform: ready → [backlog message…] → live →
// [live message…]. Since is the unix second the backlog started from (0 when
// there was no backlog, i.e. a live-only connect); Truncated is true when the
// requested resume point predated maxCatchupWindow so older messages were
// dropped. A worker can ignore this frame entirely (older SDKs do).
type liveEvent struct {
	Since     int64 `json:"since"`
	Truncated bool  `json:"truncated"`
}

// resumeWatermark decides where the stream starts delivering from, in priority:
//
//	?live=1          → now (explicit live-only; ignore the server cursor)
//	?since=<unix>    → that instant (client override), clamped to the window
//	cursor_at (>0)   → resume where delivery last stopped (the stateless-worker
//	                   default); a 0/epoch cursor = an admin "Reset replay" →
//	                   replay the whole window
//	otherwise        → now (first-ever connect, no stored position)
//
// It returns the resume instant, whether that implies replaying a backlog
// (catchUp), and whether the clamp dropped older messages (truncated). It is a
// pure function of (connector, query, now) so the policy is unit-testable
// without a DB or an HTTP server.
func resumeWatermark(conn Connector, q url.Values, now time.Time) (resumeFrom time.Time, catchUp, truncated bool) {
	windowStart := now.Add(-maxCatchupWindow)
	// clamp pins a requested instant into [windowStart, now]: older than the
	// window snaps forward (and flags truncation); a future instant snaps to now.
	clamp := func(t time.Time) (time.Time, bool) {
		switch {
		case t.Before(windowStart):
			return windowStart, true
		case t.After(now):
			return now, false
		default:
			return t, false
		}
	}
	if q.Get("live") == "1" {
		return now, false, false
	}
	if v := q.Get("since"); v != "" {
		// A malformed since is ignored (fall through to the cursor) rather than
		// silently replaying the whole window.
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			rf, tr := clamp(time.Unix(sec, 0))
			return rf, rf.Before(now), tr
		}
	}
	if conn.CursorAt != nil {
		rf, tr := clamp(*conn.CursorAt)
		return rf, rf.Before(now), tr
	}
	return now, false, false
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
	// FIX1 N2 (code-review follow-up): the read feed must obey the same
	// membership gate as the signed POST actions in authed() — otherwise an
	// admin who removes/bans the bot's synthetic member stops its writes but it
	// keeps reading every channel here. Same 404 (no oracle) as a bad signature.
	if h.MemberActive != nil && !h.MemberActive(r.Context(), conn.CommunityID, conn.UserID) {
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

	// Presence: show the member online while the worker is attached. The seam
	// keeps the member fresh on a heartbeat (the roster is TTL-based) and the
	// cleanup marks it offline on disconnect.
	if h.Presence != nil {
		if cleanup := h.Presence(conn.CommunityID, conn.UserID, conn.Name, conn.AvatarURL); cleanup != nil {
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

	// Decide where to start: the server-owned cursor (stateless-worker resume),
	// an explicit ?since= / ?live=1 override, or now (first connect). See
	// resumeWatermark.
	now := time.Now()
	resumeFrom, catchUp, truncated := resumeWatermark(conn, r.URL.Query(), now)
	// Align the watermark to whole seconds: created_at is stored as a Unix second,
	// so a sub-second resumeFrom (e.g. time.Now() on a first connect) would sit
	// strictly after a message created in that SAME second. drainChannel advances
	// maxTS from wm and rebuilds the boundary-second seen-set with created_at ==
	// maxTS; a sub-second wm makes that comparison miss every same-second message,
	// so it's delivered, never added to seen, and re-delivered on the next event.
	// Truncating closes that gap (the seen-set is the dedupe; the watermark is just
	// the second-granular floor).
	resumeFrom = resumeFrom.Truncate(time.Second)

	// Does the resume point reflect content we've ALREADY delivered — a
	// stateless-worker cursor resume, or a live-only first connect — rather than
	// an explicit ?since= replay request? It matters for the boundary second:
	// created_at is unix-second granular and the watermark query is inclusive
	// (>=), so the messages sitting exactly AT the resume second were already sent
	// last session. If we don't mark them seen, every idle reconnect re-delivers
	// that whole second — the "I keep getting all messages from X on every
	// reconnect" bug. An explicit ?since= is the opposite: a deliberate replay, so
	// we must NOT seed (the caller wants everything at/after that instant).
	q := r.URL.Query()
	fromCursor := q.Get("live") != "1" && q.Get("since") == "" && conn.CursorAt != nil && conn.CursorAt.Unix() > 0
	seedBoundary := !catchUp || fromCursor

	// Per-channel watermark (inclusive) + a seen-set for the boundary second so
	// same-second messages are neither missed nor duplicated. When seedBoundary,
	// seed seen with the rows at exactly the resume second (already delivered);
	// the catch-up drain then delivers only what's strictly newer. For an explicit
	// ?since= replay seen starts empty so the boundary second IS delivered.
	wm := make(map[string]time.Time, len(subs))
	seen := make(map[string]map[string]bool, len(subs))
	for id := range subs {
		wm[id] = resumeFrom
		seen[id] = map[string]bool{}
		if seedBoundary {
			if existing, err := h.ChatRepo.ListAfter(r.Context(), id, resumeFrom, streamBatchLimit); err == nil {
				for _, m := range existing {
					// Only the boundary (resume) second — rows strictly later are
					// genuine backlog the drain must still deliver.
					if m.CreatedAt.Unix() == resumeFrom.Unix() {
						seen[id][m.ID] = true
					}
				}
			}
		}
	}

	// Advance the server-owned resume cursor on close, ONCE, to the furthest
	// second delivered, so the next reconnect (no params) resumes here — the
	// "almost stateless worker" contract. Detached ctx: r.Context() is already
	// cancelled by the time the stream ends. Registered after wm is built so the
	// closure observes the fully-advanced map at close time.
	defer func() {
		var maxSec int64
		for _, t := range wm {
			if u := t.Unix(); u > maxSec {
				maxSec = u
			}
		}
		if maxSec > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.Repo.SetCursor(ctx, conn.CommunityID, conn.ID, maxSec); err != nil {
				h.Log.Warn("connectors: persist cursor", "connector", conn.ID, "err", err)
			}
		}
	}()

	// Catch-up: flush the backlog the worker missed, in order, BEFORE going live.
	// (Skipped in live-only mode — running it there would clear the pre-seeded
	// boundary-second seen-set and risk re-delivering the connect second.)
	if catchUp {
		for id := range subs {
			if !h.drainChannel(r.Context(), w, flush, conn, subs, wm, seen, id) {
				return
			}
		}
	}
	// Boundary marker: history (if any) flushed, everything after this is live.
	live := liveEvent{}
	if catchUp {
		live.Since, live.Truncated = resumeFrom.Unix(), truncated
	}
	if !writeFrame(w, flush, "live", live) {
		return
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
// advances that channel's watermark + seen-set. It loops so a burst larger than
// one batch is fully delivered, stopping when a batch is short (caught up) or
// makes no progress (a single second saturated beyond the batch limit — an
// extreme edge; the next event re-drains it). Returns false if the client
// connection broke (caller should return).
func (h *Handler) drainChannel(ctx context.Context, w http.ResponseWriter, flush func(), conn Connector,
	subs map[string]chat.Channel, wm map[string]time.Time, seen map[string]map[string]bool, channelID string) bool {

	ch := subs[channelID]
	for {
		msgs, err := h.ChatRepo.ListAfter(ctx, channelID, wm[channelID], streamBatchLimit)
		if err != nil {
			h.Log.Warn("connectors: stream load", "connector", conn.ID, "channel", channelID, "err", err)
			return true // transient; keep the stream open
		}
		if len(msgs) == 0 {
			return true
		}
		// Stop at the first still-generating chat-agent bubble. A KindBot message
		// is inserted as an EMPTY placeholder and its body is streamed in over
		// ~100ms ticks (chatagents.ChannelRunner → UpdateBotBody) WITHOUT changing
		// created_at. Delivering it now would push body_md/body_html="" and the
		// once-per-id seen-set would never correct it (the reported bug). So we
		// hold the watermark at the placeholder: nothing at or after it is
		// delivered until gen_status flips off, at which point the next chat event
		// re-drains and delivers it — in order — with the final body. An
		// interrupted/done bubble has its final body already, so it is NOT held.
		batch := msgs
		held := false
		for i, m := range msgs {
			if m.Kind == chat.KindBot && m.GenStatus == chat.GenGenerating {
				batch, held = msgs[:i], true
				break
			}
		}
		maxTS := wm[channelID]
		fresh := 0
		for _, m := range batch {
			if m.CreatedAt.After(maxTS) {
				maxTS = m.CreatedAt
			}
			if seen[channelID][m.ID] {
				continue // already delivered (boundary-second dedupe)
			}
			fresh++
			if !h.deliver(ctx, w, flush, conn, ch, m) {
				return false
			}
		}
		// Advance the watermark and rebuild the seen-set to exactly the ids at the
		// new boundary second, so the next inclusive query skips them but still
		// catches a fresh same-second arrival. Only the DELIVERED prefix counts —
		// a held generating bubble must stay unseen so it's re-delivered later.
		wm[channelID] = maxTS
		next := map[string]bool{}
		for _, m := range batch {
			if m.CreatedAt.Equal(maxTS) {
				next[m.ID] = true
			}
		}
		seen[channelID] = next
		// A generating bubble truncated the batch → stop; we must not advance past
		// it, and the next chat event re-drains from this watermark. Otherwise the
		// usual termination: a short batch = caught up; a full batch with nothing
		// fresh = one second saturated beyond the limit (re-drained next event).
		if held {
			return true
		}
		if len(msgs) < streamBatchLimit || fresh == 0 {
			return true
		}
	}
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
