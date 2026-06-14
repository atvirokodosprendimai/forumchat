// Package push wires the Web Push (VAPID) flow into forumchat.
//
//   - Subscribe — browser registers the service worker, calls
//     PushManager.subscribe with the VAPID public key, POSTs the
//     resulting subscription + per-event settings to /push/subscribe.
//   - Send — server enqueues a notification by user (or set of users),
//     looks up every subscription, checks per-event opt-in, and
//     dispatches via SherClockHolmes/webpush-go.
//
// The keys are derived from VAPID_PUBLIC / VAPID_PRIVATE if set; else
// they are auto-generated once and persisted to VAPID_KEYS_FILE so
// reloads keep working. Production deployments should pin them via
// env so a new disk doesn't invalidate every subscription.
package push

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Settings is the per-(user, community) opt-in map. Missing keys
// default to true on the client; the server treats missing keys the
// same way so unknown future kinds opt-in by default.
type Settings map[string]bool

// Subscription mirrors what the browser hands back from
// PushManager.subscribe. Stored verbatim per row.
type Subscription struct {
	ID          string
	UserID      string
	CommunityID string
	Endpoint    string
	P256dh      string
	AuthKey     string
	UserAgent   string
	Settings    Settings
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LoadOrCreateVAPID returns the configured public/private VAPID pair.
// If both env values are non-empty they win. Otherwise the keys are
// read from path; if that file doesn't exist a fresh pair is generated
// and written so subsequent boots are stable.
func LoadOrCreateVAPID(envPub, envPriv, path string, log *slog.Logger) (pub, priv string, err error) {
	envPub = strings.TrimSpace(envPub)
	envPriv = strings.TrimSpace(envPriv)
	if envPub != "" && envPriv != "" {
		return envPub, envPriv, nil
	}
	// File-backed cache: same shape as env, marshalled as small JSON.
	type vapidFile struct {
		Public  string `json:"public"`
		Private string `json:"private"`
	}
	if b, rerr := os.ReadFile(path); rerr == nil {
		var v vapidFile
		if jerr := json.Unmarshal(b, &v); jerr == nil && v.Public != "" && v.Private != "" {
			return v.Public, v.Private, nil
		}
	}
	// Generate fresh.
	pri, pubKey, gerr := webpush.GenerateVAPIDKeys()
	if gerr != nil {
		return "", "", fmt.Errorf("generate VAPID: %w", gerr)
	}
	out, _ := json.Marshal(vapidFile{Public: pubKey, Private: pri})
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		log.Warn("vapid keys dir", "err", mkErr)
	}
	if werr := os.WriteFile(path, out, 0o600); werr != nil {
		log.Warn("vapid keys write", "path", path, "err", werr)
	} else {
		log.Info("vapid keys generated", "path", path)
	}
	return pubKey, pri, nil
}

// Repo handles persistence for push_subscriptions.
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// Upsert replaces or inserts a row keyed by (user, community, endpoint).
// settings is marshalled to JSON.
func (r *Repo) Upsert(ctx context.Context, s Subscription) error {
	settingsJSON, err := json.Marshal(s.Settings)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	now := time.Now().Unix()
	_, err = r.DB.ExecContext(ctx, `
		INSERT INTO push_subscriptions
		    (id, user_id, community_id, endpoint, p256dh, auth_key, user_agent, settings_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, community_id, endpoint) DO UPDATE SET
		    p256dh        = excluded.p256dh,
		    auth_key      = excluded.auth_key,
		    user_agent    = excluded.user_agent,
		    settings_json = excluded.settings_json,
		    updated_at    = excluded.updated_at
	`, s.ID, s.UserID, s.CommunityID, s.Endpoint, s.P256dh, s.AuthKey, s.UserAgent, string(settingsJSON), now, now)
	return err
}

// DeleteByEndpoint removes a subscription. Used on unsubscribe and on
// 410/404 responses from the push service.
func (r *Repo) DeleteByEndpoint(ctx context.Context, userID, endpoint string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`,
		userID, endpoint)
	return err
}

// UpdateSettings only patches the settings JSON without touching keys.
func (r *Repo) UpdateSettings(ctx context.Context, userID, communityID string, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
		UPDATE push_subscriptions
		SET settings_json = ?, updated_at = ?
		WHERE user_id = ? AND community_id = ?
	`, string(b), time.Now().Unix(), userID, communityID)
	return err
}

// SettingsFor returns the merged settings the user has for a community.
// When several subscriptions exist (multiple devices) the most recent
// row wins for the response. Empty map when no subscription.
func (r *Repo) SettingsFor(ctx context.Context, userID, communityID string) (Settings, bool, error) {
	var s string
	err := r.DB.QueryRowContext(ctx, `
		SELECT settings_json FROM push_subscriptions
		WHERE user_id = ? AND community_id = ?
		ORDER BY updated_at DESC LIMIT 1
	`, userID, communityID).Scan(&s)
	if err == sql.ErrNoRows {
		return Settings{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out Settings
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = Settings{}
	}
	return out, true, nil
}

// ListForCommunityWith returns every subscription in this community whose
// settings have the kind opted-in (treated as enabled when the key is
// absent — opt-in by default).
func (r *Repo) ListForCommunityWith(ctx context.Context, communityID, kind string) ([]Subscription, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, user_id, community_id, endpoint, p256dh, auth_key, user_agent, settings_json
		FROM push_subscriptions
		WHERE community_id = ?
	`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Subscription, 0, 32)
	for rows.Next() {
		var s Subscription
		var settingsJSON string
		if err := rows.Scan(&s.ID, &s.UserID, &s.CommunityID, &s.Endpoint, &s.P256dh, &s.AuthKey, &s.UserAgent, &settingsJSON); err != nil {
			return nil, err
		}
		var m Settings
		_ = json.Unmarshal([]byte(settingsJSON), &m)
		if m == nil {
			m = Settings{}
		}
		s.Settings = m
		if v, ok := m[kind]; ok && !v {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListForUsersWith returns subscriptions for a specific user set in this
// community, gated by per-kind setting.
func (r *Repo) ListForUsersWith(ctx context.Context, communityID, kind string, userIDs []string) ([]Subscription, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(userIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := []any{communityID}
	for _, u := range userIDs {
		args = append(args, u)
	}
	q := `
		SELECT id, user_id, community_id, endpoint, p256dh, auth_key, user_agent, settings_json
		FROM push_subscriptions
		WHERE community_id = ? AND user_id IN (` + placeholders + `)
	`
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		var settingsJSON string
		if err := rows.Scan(&s.ID, &s.UserID, &s.CommunityID, &s.Endpoint, &s.P256dh, &s.AuthKey, &s.UserAgent, &settingsJSON); err != nil {
			return nil, err
		}
		var m Settings
		_ = json.Unmarshal([]byte(settingsJSON), &m)
		if m == nil {
			m = Settings{}
		}
		s.Settings = m
		if v, ok := m[kind]; ok && !v {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Notification is the payload the service worker receives.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// Sender dispatches notifications via web push.
type Sender struct {
	Repo       *Repo
	Public     string
	Private    string
	Subject    string
	Log        *slog.Logger
	HTTPClient *http.Client
}

// SendToCommunity pushes to every opted-in subscription in the community.
func (s *Sender) SendToCommunity(ctx context.Context, communityID, kind string, n Notification) {
	subs, err := s.Repo.ListForCommunityWith(ctx, communityID, kind)
	if err != nil {
		s.Log.Warn("push list community", "err", err)
		return
	}
	s.dispatch(ctx, subs, n)
}

// SendToUsers pushes to a specific set of users (e.g. @mentions).
func (s *Sender) SendToUsers(ctx context.Context, communityID, kind string, userIDs []string, n Notification) {
	subs, err := s.Repo.ListForUsersWith(ctx, communityID, kind, userIDs)
	if err != nil {
		s.Log.Warn("push list users", "err", err)
		return
	}
	s.dispatch(ctx, subs, n)
}

func (s *Sender) dispatch(ctx context.Context, subs []Subscription, n Notification) {
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return
	}
	for _, sub := range subs {
		opts := &webpush.Options{
			Subscriber:      s.Subject,
			VAPIDPublicKey:  s.Public,
			VAPIDPrivateKey: s.Private,
			TTL:             60 * 60 * 24, // 1 day
			HTTPClient:      s.HTTPClient,
		}
		resp, perr := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				P256dh: sub.P256dh,
				Auth:   sub.AuthKey,
			},
		}, opts)
		if perr != nil {
			s.Log.Warn("push send", "endpoint", trimEndpoint(sub.Endpoint), "err", perr)
			continue
		}
		if resp != nil {
			_ = resp.Body.Close()
			// 410 Gone / 404 Not Found → the browser unsubscribed. Drop the row.
			if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
				_ = s.Repo.DeleteByEndpoint(ctx, sub.UserID, sub.Endpoint)
			}
		}
	}
}

func trimEndpoint(e string) string {
	if len(e) > 64 {
		return e[:64] + "…"
	}
	return e
}

// ErrNotConfigured is returned when push handlers are hit without VAPID.
var ErrNotConfigured = errors.New("push not configured")
