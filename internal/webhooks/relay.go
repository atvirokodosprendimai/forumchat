package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Relay is the outbound side: it delivers a human chat message to every
// matching direction='out' webhook as a JSON POST. Best-effort, no retry queue
// (v1) — non-2xx responses are logged and stamped on the row.
type Relay struct {
	Repo   *Repo
	Client *http.Client
	Log    *slog.Logger
	// ResolveAttachments, if set, resolves a message's upload IDs into fetchable
	// outbound attachments (shared-signed URL + metadata). Optional — nil means
	// attachments are never included in the payload. Wired in main.go so this
	// package stays decoupled from the uploads store.
	ResolveAttachments func(ctx context.Context, uploadIDs []string) []OutboundAttachment
}

// OutboundAttachment is one file attached to a relayed message, as carried in
// the generic provider's payload. URL is a shared-signed, session-less link a
// downstream consumer can fetch directly.
type OutboundAttachment struct {
	URL  string `json:"url"`
	MIME string `json:"mime"`
	Name string `json:"name"`
}

// NewRelay returns a Relay with a short-timeout HTTP client.
func NewRelay(repo *Repo, log *slog.Logger) *Relay {
	return &Relay{
		Repo:   repo,
		Client: &http.Client{Timeout: 10 * time.Second},
		Log:    log,
	}
}

// OutboundMsg is the full content of one outbound relay. The Thread* fields are
// set only for forum-originated relays (thread announce + reply); chat relays
// leave them zero and the generic payload omits the thread keys entirely.
type OutboundMsg struct {
	CommunityID string
	ChannelID   string
	ChannelName string
	Author      string
	BodyMD      string
	ThreadID    string // forumchat forum thread id (forum relays only)
	MessageID   string // forum post id; "" for the thread-opening announce
	Subject     string // thread subject (forum relays only)
	ThreadRoot  bool   // true = this is the thread's opening message
	// AttachmentUploadIDs are upload row ids to relay as media (chat sends only).
	AttachmentUploadIDs []string
	// Attachments is the resolved form (URL + metadata), filled by dispatch
	// before encoding via Relay.ResolveAttachments.
	Attachments []OutboundAttachment
}

// Dispatch fires-and-forgets the relay of one chat message. It detaches from
// the request lifecycle (own background context) so a client disconnect doesn't
// cancel delivery. Safe to call with no matching webhooks — it returns after
// one cheap query. Callers pass member-driven content: KindUser sends +
// forwards, agent share-to-channel, slash-command output (/summary, /prompt,
// shared /search results), and forum new-thread announcements — never a
// KindWebhook bot post, so an inbound bot post never triggers an outbound relay
// (no echo loop).
func (r *Relay) Dispatch(communityID, channelID, authorName, bodyMD, channelName string, attachmentUploadIDs []string) {
	r.dispatch(OutboundMsg{
		CommunityID:         communityID,
		ChannelID:           channelID,
		ChannelName:         channelName,
		Author:              authorName,
		BodyMD:              bodyMD,
		AttachmentUploadIDs: attachmentUploadIDs,
	})
}

// DispatchForum relays forum-thread content, carrying the thread identity so a
// downstream bridge can group messages into one external thread (e.g. a Matrix
// m.thread). Echo guard is the caller's: only human-authored forum content is
// dispatched (inbound-webhook posts are bot posts and never reach here).
func (r *Relay) DispatchForum(m OutboundMsg) { r.dispatch(m) }

// dispatch is the shared relay loop for chat and forum messages.
func (r *Relay) dispatch(m OutboundMsg) {
	if r == nil || r.Repo == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		hooks, err := r.Repo.OutboundForChannel(ctx, m.CommunityID, m.ChannelID)
		if err != nil {
			r.Log.Error("webhooks relay: load outbound", "err", err)
			return
		}
		if r.ResolveAttachments != nil && len(m.AttachmentUploadIDs) > 0 {
			m.Attachments = r.ResolveAttachments(ctx, m.AttachmentUploadIDs)
		}
		for _, wh := range hooks {
			payload := encodePayload(wh.Provider, m)
			status := r.post(ctx, wh.TargetURL, payload)
			if err := r.Repo.Stamp(ctx, wh.ID, status); err != nil {
				r.Log.Warn("webhooks relay: stamp", "err", err)
			}
		}
	}()
}

// post sends payload to url and returns a short status string for the row.
func (r *Relay) post(ctx context.Context, url string, payload []byte) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "bad-url"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		r.Log.Warn("webhooks relay: deliver failed", "url", url, "err", err)
		return "error"
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.Log.Warn("webhooks relay: non-2xx", "url", url, "status", resp.StatusCode)
	}
	return strconv.Itoa(resp.StatusCode)
}

// encodePayload builds the JSON body for a provider. slack/discord both consume
// {"text":...} (Discord accepts it via compat); generic carries structured
// fields so a downstream system can route on them. For forum-originated relays
// the generic payload also carries the thread identity (thread_id, subject,
// thread_root, message_id) so a bridge can mirror the conversation into one
// external thread; chat relays omit those keys (payload stays byte-stable).
// When a chat message has attachments the generic payload also carries an
// `attachments` array (url + mime + name); omitted when empty.
func encodePayload(provider string, m OutboundMsg) []byte {
	switch provider {
	case "slack", "discord":
		text := m.Author + ": " + m.BodyMD
		if m.ChannelName != "" {
			text = "[#" + m.ChannelName + "] " + text
		}
		b, _ := json.Marshal(map[string]string{"text": text})
		return b
	default: // generic
		out := map[string]any{
			"community":  m.CommunityID,
			"channel":    m.ChannelName,
			"author":     m.Author,
			"body_md":    m.BodyMD,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		if m.ThreadID != "" {
			out["thread_id"] = m.ThreadID
			out["subject"] = m.Subject
			out["thread_root"] = m.ThreadRoot
			if m.MessageID != "" {
				out["message_id"] = m.MessageID
			}
		}
		if len(m.Attachments) > 0 {
			out["attachments"] = m.Attachments
		}
		b, _ := json.Marshal(out)
		return b
	}
}
