// Package connector is a tiny, dependency-free client for forumchat "external
// connector" chat bots. A connector lets an arbitrary external program take part
// in a community's chat as if it were a member: it holds open one signed SSE
// stream to receive messages (Client.Stream) and POSTs signed requests to send
// them and run granted moderation actions (Send, Delete, Ban, Rename).
//
// You get the connector id + secret + base URL once, from the community admin
// page (/c/{slug}/admin/connectors, reveal-on-create). Hand them to New and the
// client signs every request for you:
//
//	c := connector.New("https://chat.example.com", id, secret)
//
//	// receive
//	go c.Stream(ctx, connector.Handlers{
//		OnMessage: func(e connector.Event) { fmt.Println(e.Nick, e.BodyMD) },
//	})
//
//	// send
//	c.Send(ctx, "support", "hello from the outside")
//
// The wire is JSON, not HTML — a connector's consumer is a machine, so the SDK
// hands you plain structs, never DOM fragments. The auth model: read = a signed
// URL (the SDK builds it from your secret), write = a per-request body HMAC in
// the X-Signature header. One secret powers both; rotating it server-side
// revokes both at once.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client talks to one connector's endpoints. It is safe for concurrent use: the
// streaming read and the signing POSTs share nothing mutable. The zero value is
// not usable — construct it with New so the HTTP client and base URL are set up
// correctly (in particular, a stream-safe client with no overall Timeout).
type Client struct {
	// BaseURL is the forumchat origin, e.g. "https://chat.example.com" (no
	// trailing /bots path, no trailing slash). New normalises a trailing slash.
	BaseURL string
	// ID is the public connector id (it appears in the URL).
	ID string
	// Secret is the private per-connector HMAC key. Treat it like a password.
	Secret string
	// HTTP is the transport. It MUST NOT set an overall Timeout, or it would cut
	// the long-lived stream mid-flight; per-request deadlines come from the ctx
	// passed to each call. New supplies a suitable client.
	HTTP *http.Client
}

// New returns a Client for the connector identified by id+secret at baseURL. The
// HTTP client it installs has no Timeout (so Stream can stay open indefinitely);
// bound the lifetime of any single call through its context instead.
func New(baseURL, id, secret string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		ID:      id,
		Secret:  secret,
		HTTP:    &http.Client{}, // no Timeout — see field doc
	}
}

// ---- wire types (mirror the server's internal/connectors StreamEvent) --------

// Channel is one chat channel the connector is subscribed to, as named in the
// ready handshake.
type Channel struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Ready is the one-shot handshake frame (event: ready) the server sends first.
// A stateless worker can configure itself from it alone — who it is (Nick) and
// which channels it will receive.
type Ready struct {
	Connector string    `json:"connector"`
	Nick      string    `json:"nick"`
	Channels  []Channel `json:"channels"`
}

// Attachment is one file on a streamed message: a directly fetchable,
// shared-signed URL plus metadata.
type Attachment struct {
	URL  string `json:"url"`
	MIME string `json:"mime"`
	Name string `json:"name"`
}

// Event is one chat message delivered over the stream (event: message). It is
// the stable wire contract — field names match the server byte-for-byte. The
// connector's own posts are never echoed back, and soft-deleted / system rows
// are filtered out server-side, so every Event is real, deliverable content.
type Event struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`    // channel slug
	ChannelID string `json:"channel_id"` // stable channel id
	Nick      string `json:"nick"`       // author display name
	AuthorID  string `json:"author_id"`  // stable user id (for Ban); "" for author-less rows
	Kind      string `json:"kind"`       // user | webhook | bot
	BodyMD    string `json:"body_md"`    // markdown source
	BodyHTML  string `json:"body_html"`  // rendered, sanitized HTML
	Mentioned bool   `json:"mentioned"`  // body @mentions THIS connector
	ReplyTo   string `json:"reply_to,omitempty"`
	CreatedAt string `json:"created_at"` // RFC3339 UTC

	Attachments []Attachment `json:"attachments,omitempty"`
}

// Handlers are the per-frame callbacks for Stream. Both are optional; a nil
// callback drops that frame type. They run on Stream's goroutine in delivery
// order, so they must not block for long (offload heavy work to your own
// goroutine/queue).
type Handlers struct {
	OnReady   func(Ready)
	OnMessage func(Event)
}

// ---- read: the signed SSE stream ---------------------------------------------

// StreamURL builds the signed GET URL for the read stream. exp is a Unix expiry
// after which the URL stops working; pass 0 for a non-expiring URL. The
// signature binds the URL to this connector id, so it can't be reused for
// another connector or extended to a later expiry.
func (c *Client) StreamURL(exp int64) string {
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", streamSig(c.Secret, c.ID, exp))
	return fmt.Sprintf("%s/bots/%s/stream?%s", c.BaseURL, c.ID, q.Encode())
}

// Stream opens the read stream and dispatches frames to h until ctx is cancelled
// or the connection ends. It blocks for the life of the stream and returns:
//
//   - nil on a clean server-side close (the worker should usually reconnect),
//   - ctx.Err() when the caller cancels,
//   - a non-nil error on an HTTP error status or a transport failure.
//
// Stream does NOT reconnect on its own — that policy (and its backoff) belongs
// to the caller, so it stays a few lines (see examples/tinychat). exp is the
// signed-URL expiry; 0 means non-expiring.
func (c *Client) Stream(ctx context.Context, h Handlers, exp int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.StreamURL(exp), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiErrorFrom(resp)
	}

	// scanSSE drives the read; each frame is decoded by event name and handed to
	// the matching handler. A handler returning is the only thing that advances
	// the loop, so the callback returns true to keep going (cancellation comes
	// from ctx aborting the underlying read, not from the callback).
	scanErr := scanSSE(resp.Body, func(f sseFrame) bool {
		switch f.event {
		case "ready":
			if h.OnReady != nil {
				var rdy Ready
				if json.Unmarshal(f.data, &rdy) == nil {
					h.OnReady(rdy)
				}
			}
		case "message":
			if h.OnMessage != nil {
				var ev Event
				if json.Unmarshal(f.data, &ev) == nil {
					h.OnMessage(ev)
				}
			}
		}
		return true
	})
	// Translate the read outcome into the documented contract: caller cancel wins,
	// a clean EOF is a normal close (nil), anything else is the real failure.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanErr == io.EOF {
		return nil
	}
	return scanErr
}

// ---- write: signed POSTs (send + moderation) ---------------------------------

// SendResult is the server's reply to a successful Send: the new message id and
// the channel slug it landed in (useful when Send defaulted the channel).
type SendResult struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
}

// Send posts body as the connector's member into the given channel slug. Pass an
// empty channel to use the connector's sole subscribed channel (the server
// rejects an empty channel when the connector has more than one). The connector
// must hold the "send" capability. To reply to a message, use Reply.
func (c *Client) Send(ctx context.Context, channel, body string) (SendResult, error) {
	return c.Reply(ctx, channel, body, "")
}

// Reply is Send with a parent message id, so the new message threads under it.
// The parent must be a live message in the same channel (the server validates
// this); an empty replyTo behaves exactly like Send.
func (c *Client) Reply(ctx context.Context, channel, body, replyTo string) (SendResult, error) {
	var out SendResult
	err := c.post(ctx, "send", map[string]string{
		"channel":  channel,
		"body":     body,
		"reply_to": replyTo,
	}, &out)
	return out, err
}

// Delete soft-deletes a chat message by id (hidden from everyone). The connector
// must hold the "delete" capability and the message must be in one of its
// allowed channels. Returns nil on success.
func (c *Client) Delete(ctx context.Context, messageID string) error {
	return c.post(ctx, "delete", map[string]string{"message_id": messageID}, nil)
}

// Ban bans a member by user id (the AuthorID surfaced on stream Events). hours is
// the ban duration; 0 means permanent. The connector must hold the "ban"
// capability; the server refuses to ban an admin/owner or the connector itself.
func (c *Client) Ban(ctx context.Context, userID string, hours int) error {
	return c.post(ctx, "ban", map[string]any{"user_id": userID, "hours": hours}, nil)
}

// Rename renames a channel (by slug) to name. The connector must hold the
// "rename" capability; the server refuses to rename the default #general.
func (c *Client) Rename(ctx context.Context, channel, name string) error {
	return c.post(ctx, "rename", map[string]string{"channel": channel, "name": name}, nil)
}

// post is the one signed-write primitive every action shares (Send, Delete, Ban,
// Rename): marshal req, sign the exact bytes with the connector secret, POST to
// /bots/<id>/<action>, and on a 2xx decode the JSON reply into out (out may be
// nil to discard it). A non-2xx becomes an *APIError carrying the status and the
// server's message. Keeping this single helper means a new action is one thin
// wrapper, not another copy of the signing dance.
func (c *Client) post(ctx context.Context, action string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/bots/%s/%s", c.BaseURL, c.ID, action)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Sign the EXACT bytes we send — the server recomputes the HMAC over the raw
	// body, so any divergence (re-marshal, whitespace) would fail verification.
	httpReq.Header.Set("X-Signature", signBody(c.Secret, body))

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiErrorFrom(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// APIError is a non-2xx response from the connector endpoints. The status code
// distinguishes the failure modes the server documents — 404 unknown/disabled
// connector (or bad stream signature), 401 bad/missing send signature, 403
// capability-not-granted or channel-not-allowed, 400 a malformed request — and
// Message is the server's short plaintext explanation.
type APIError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		return fmt.Sprintf("connector: http %d", e.Status)
	}
	return fmt.Sprintf("connector: http %d: %s", e.Status, msg)
}

// apiErrorFrom builds an *APIError from a response, reading a bounded slice of
// the body for the message (the server's error bodies are short plaintext).
func apiErrorFrom(resp *http.Response) *APIError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	return &APIError{Status: resp.StatusCode, Message: string(bytes.TrimSpace(b))}
}
