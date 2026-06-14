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
	"crypto/rand"
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

func cryptoRead(b []byte) (int, error) { return rand.Read(b) }

// Settings is the per-(user, community) opt-in map. Missing keys
// default to true on the client; the server treats missing keys the
// same way so unknown future kinds opt-in by default.
type Settings map[string]bool

// Subscription mirrors what the browser hands back from
// PushManager.subscribe. Stored verbatim per row.
type Subscription struct {
	ID             string
	UserID         string
	CommunityID    string
	Endpoint       string
	P256dh         string
	AuthKey        string
	UserAgent      string
	Settings       Settings
	DigestMinutes  int   // 0 = immediate; >0 = batch every N minutes
	DigestLastAt   int64 // unix seconds of the last digest sent
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PendingRow is a buffered notification waiting for the digest worker
// to flush it to a recipient.
type PendingRow struct {
	ID          string
	UserID      string
	CommunityID string
	Kind        string
	Title       string
	Body        string
	URL         string
	CreatedAt   int64
}

// DigestKey is the (user, community) pair the worker dispatches against
// when an interval has elapsed and pending rows exist.
type DigestKey struct {
	UserID      string
	CommunityID string
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
		    (id, user_id, community_id, endpoint, p256dh, auth_key, user_agent, settings_json, digest_minutes, digest_last_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, community_id, endpoint) DO UPDATE SET
		    p256dh        = excluded.p256dh,
		    auth_key      = excluded.auth_key,
		    user_agent    = excluded.user_agent,
		    settings_json = excluded.settings_json,
		    digest_minutes = excluded.digest_minutes,
		    updated_at    = excluded.updated_at
	`, s.ID, s.UserID, s.CommunityID, s.Endpoint, s.P256dh, s.AuthKey, s.UserAgent, string(settingsJSON), s.DigestMinutes, s.DigestLastAt, now, now)
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

// UpdateSettings patches the settings JSON + digest_minutes on every
// subscription this user holds in the community.
func (r *Repo) UpdateSettings(ctx context.Context, userID, communityID string, s Settings, digestMinutes int) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
		UPDATE push_subscriptions
		SET settings_json = ?, digest_minutes = ?, updated_at = ?
		WHERE user_id = ? AND community_id = ?
	`, string(b), digestMinutes, time.Now().Unix(), userID, communityID)
	return err
}

// AddPending buffers an event for later delivery via the digest worker.
func (r *Repo) AddPending(ctx context.Context, userID, communityID, kind, title, body, url string) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO push_pending (id, user_id, community_id, kind, title, body, url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, randomID(), userID, communityID, kind, title, body, url, time.Now().Unix())
	return err
}

// ListPending returns every pending event for one (user, community) in
// insertion order so the digest preserves "this happened then that".
func (r *Repo) ListPending(ctx context.Context, userID, communityID string) ([]PendingRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, user_id, community_id, kind, title, body, url, created_at
		FROM push_pending
		WHERE user_id = ? AND community_id = ?
		ORDER BY created_at ASC
	`, userID, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRow
	for rows.Next() {
		var p PendingRow
		if err := rows.Scan(&p.ID, &p.UserID, &p.CommunityID, &p.Kind, &p.Title, &p.Body, &p.URL, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingByIDs drops a set of pending rows (used after the digest
// worker successfully consolidated and dispatched them).
func (r *Repo) DeletePendingByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM push_pending WHERE id IN (`+placeholders+`)`, args...)
	return err
}

// DueDigests returns every (user, community) pair where:
//   - the user has at least one subscription with digest_minutes > 0
//   - there are pending rows queued for that pair
//   - the most-recent digest for that pair was sent more than
//     digest_minutes * 60 seconds ago
func (r *Repo) DueDigests(ctx context.Context, now int64) ([]DigestKey, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT s.user_id, s.community_id
		FROM push_subscriptions s
		WHERE s.digest_minutes > 0
		GROUP BY s.user_id, s.community_id
		HAVING (? - MAX(s.digest_last_at)) >= MAX(s.digest_minutes) * 60
		   AND EXISTS (
		     SELECT 1 FROM push_pending p
		     WHERE p.user_id = s.user_id AND p.community_id = s.community_id
		   )
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DigestKey
	for rows.Next() {
		var k DigestKey
		if err := rows.Scan(&k.UserID, &k.CommunityID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SubsForUserCommunity returns every subscription the user holds in
// this community regardless of digest_minutes — used by the worker
// when fanning out the digest to every device.
func (r *Repo) SubsForUserCommunity(ctx context.Context, userID, communityID string) ([]Subscription, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, user_id, community_id, endpoint, p256dh, auth_key, user_agent, settings_json, digest_minutes, digest_last_at
		FROM push_subscriptions
		WHERE user_id = ? AND community_id = ?
	`, userID, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		var settingsJSON string
		if err := rows.Scan(&s.ID, &s.UserID, &s.CommunityID, &s.Endpoint, &s.P256dh, &s.AuthKey, &s.UserAgent, &settingsJSON, &s.DigestMinutes, &s.DigestLastAt); err != nil {
			return nil, err
		}
		var m Settings
		_ = json.Unmarshal([]byte(settingsJSON), &m)
		if m == nil {
			m = Settings{}
		}
		s.Settings = m
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetDigestLastAt bumps digest_last_at on every subscription for the
// (user, community) so the cooldown restarts uniformly across devices.
func (r *Repo) SetDigestLastAt(ctx context.Context, userID, communityID string, ts int64) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE push_subscriptions
		SET digest_last_at = ?
		WHERE user_id = ? AND community_id = ?
	`, ts, userID, communityID)
	return err
}

// randomID is a small helper for push_pending PKs. We don't need
// cryptographic uniqueness; collision odds in a 64-bit hex are zero
// at human scale.
func randomID() string {
	var b [12]byte
	_, _ = cryptoRead(b[:])
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
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
	// Group by recipient user. If any of the user's subscriptions in
	// this community is in digest mode, the event goes into the pending
	// buffer instead of being pushed immediately. UpdateSettings keeps
	// digest_minutes uniform per (user, community), so picking the max
	// is just defensive.
	byUser := map[string][]Subscription{}
	for _, sub := range subs {
		byUser[sub.UserID] = append(byUser[sub.UserID], sub)
	}
	for _, list := range byUser {
		digest := 0
		for _, sub := range list {
			if sub.DigestMinutes > digest {
				digest = sub.DigestMinutes
			}
		}
		if digest > 0 {
			cid := list[0].CommunityID
			uid := list[0].UserID
			if err := s.Repo.AddPending(ctx, uid, cid, n.Tag, n.Title, n.Body, n.URL); err != nil {
				s.Log.Warn("push add pending", "err", err)
			}
			continue
		}
		s.SendNow(ctx, list, n)
	}
}

// SendNow pushes a notification to the given subs immediately, bypassing
// digest buffering. Exposed so the digest worker can fan out the
// consolidated payload itself.
func (s *Sender) SendNow(ctx context.Context, subs []Subscription, n Notification) {
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
		}
		// Only set HTTPClient when the caller injected one. webpush-go
		// reads through an interface, so passing a typed-nil *http.Client
		// here defeats its own nil-check and panics inside (*Client).Do.
		if s.HTTPClient != nil {
			opts.HTTPClient = s.HTTPClient
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

// DigestWorker periodically scans push_pending for ready digests,
// builds a consolidated Notification per (user, community), pushes
// it to that user's digest-mode subscriptions, then clears the
// buffer rows it consumed.
type DigestWorker struct {
	Repo     *Repo
	Sender   *Sender
	Interval time.Duration // poll interval; 30s is a fine default
	Log      *slog.Logger
}

// Start runs until ctx is cancelled. Safe to call once at boot.
func (w *DigestWorker) Start(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(w.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

func (w *DigestWorker) tick(ctx context.Context) {
	keys, err := w.Repo.DueDigests(ctx, time.Now().Unix())
	if err != nil {
		w.Log.Warn("digest due scan", "err", err)
		return
	}
	for _, k := range keys {
		pending, err := w.Repo.ListPending(ctx, k.UserID, k.CommunityID)
		if err != nil || len(pending) == 0 {
			continue
		}
		n := BuildDigest(pending)
		subs, err := w.Repo.SubsForUserCommunity(ctx, k.UserID, k.CommunityID)
		if err != nil {
			w.Log.Warn("digest subs lookup", "err", err)
			continue
		}
		// Only push to subs that opted into digest mode. An immediate
		// device for the same user has already been pinged when each
		// event fired.
		ready := make([]Subscription, 0, len(subs))
		for _, sub := range subs {
			if sub.DigestMinutes > 0 {
				ready = append(ready, sub)
			}
		}
		if len(ready) > 0 {
			w.Sender.SendNow(ctx, ready, n)
		}
		if err := w.Repo.SetDigestLastAt(ctx, k.UserID, k.CommunityID, time.Now().Unix()); err != nil {
			w.Log.Warn("digest set last_at", "err", err)
		}
		ids := make([]string, 0, len(pending))
		for _, p := range pending {
			ids = append(ids, p.ID)
		}
		if err := w.Repo.DeletePendingByIDs(ctx, ids); err != nil {
			w.Log.Warn("digest delete pending", "err", err)
		}
	}
}

// BuildDigest collapses N pending events into one Notification.
//
//   - 1 event   → exact same payload as immediate.
//   - N events  → "N new updates" title, "2 mentions, 1 new thread, ..."
//     body, no URL (the SW falls back to / on click).
func BuildDigest(rows []PendingRow) Notification {
	if len(rows) == 1 {
		r := rows[0]
		return Notification{Title: r.Title, Body: r.Body, URL: r.URL, Tag: r.Kind}
	}
	counts := map[string]int{}
	order := make([]string, 0, 6)
	for _, r := range rows {
		if _, ok := counts[r.Kind]; !ok {
			order = append(order, r.Kind)
		}
		counts[r.Kind]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], kindLabel(k, counts[k])))
	}
	return Notification{
		Title: fmt.Sprintf("%d new updates", len(rows)),
		Body:  strings.Join(parts, ", "),
		Tag:   "digest",
	}
}

func kindLabel(k string, n int) string {
	plural := n != 1
	switch k {
	case "mention":
		if plural {
			return "mentions"
		}
		return "mention"
	case "thread_new":
		if plural {
			return "new threads"
		}
		return "new thread"
	case "project_new":
		if plural {
			return "new projects"
		}
		return "new project"
	case "issue_new":
		if plural {
			return "new issues"
		}
		return "new issue"
	case "comment_new":
		if plural {
			return "new comments"
		}
		return "new comment"
	case "report":
		if plural {
			return "moderation alerts"
		}
		return "moderation alert"
	default:
		if plural {
			return "updates"
		}
		return "update"
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
