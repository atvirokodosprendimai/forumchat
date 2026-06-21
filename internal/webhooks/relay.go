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
}

// NewRelay returns a Relay with a short-timeout HTTP client.
func NewRelay(repo *Repo, log *slog.Logger) *Relay {
	return &Relay{
		Repo:   repo,
		Client: &http.Client{Timeout: 10 * time.Second},
		Log:    log,
	}
}

// Dispatch fires-and-forgets the relay of one chat message. It detaches from
// the request lifecycle (own background context) so a client disconnect doesn't
// cancel delivery. Safe to call with no matching webhooks — it returns after
// one cheap query. Only KindUser messages reach here (see chat handler wiring),
// so an inbound bot post never triggers an outbound relay (no echo loop).
func (r *Relay) Dispatch(communityID, channelID, authorName, bodyMD, channelName string) {
	if r == nil || r.Repo == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		hooks, err := r.Repo.OutboundForChannel(ctx, communityID, channelID)
		if err != nil {
			r.Log.Error("webhooks relay: load outbound", "err", err)
			return
		}
		for _, wh := range hooks {
			payload := encodePayload(wh.Provider, communityID, channelName, authorName, bodyMD)
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
// fields so a downstream system can route on them.
func encodePayload(provider, communityID, channelName, author, bodyMD string) []byte {
	switch provider {
	case "slack", "discord":
		text := author + ": " + bodyMD
		if channelName != "" {
			text = "[#" + channelName + "] " + text
		}
		b, _ := json.Marshal(map[string]string{"text": text})
		return b
	default: // generic
		b, _ := json.Marshal(map[string]string{
			"community":  communityID,
			"channel":    channelName,
			"author":     author,
			"body_md":    bodyMD,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		})
		return b
	}
}
