// Package connectors provides per-community "external chat bot" connectors: a
// long-lived, HMAC-signed SSE stream pushes realtime channel messages as JSON
// to an external worker, and a body-HMAC-signed POST lets it send back. Each
// connector is backed by a real synthetic member (own user + membership), so it
// acts as a human — roster, @mention, profile and mod-delete all apply with no
// per-feature code. It is the persistent-stream, human-identity, bidirectional
// sibling of internal/webhooks.
//
// CQRS split (AGENTS §6b): repo.go holds all SQL (this file), service.go owns
// writes, sign.go/event.go are pure helpers, stream.go/handler.go are the HTTP
// boundary.
package connectors

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/secretbox"
)

// ErrNotFound is returned when no connector matches a lookup.
var ErrNotFound = errors.New("connectors: not found")

// Capability tokens an admin can grant a connector. Send is the base ability to
// post; the rest are the same powers a member has from the chat dropdown — each
// enables a matching signed action endpoint (/bots/<id>/<cap>). The set is
// open-ended (stored as CSV) so adding a new power needs no migration — only a
// new endpoint + this constant + a KnownCapabilities entry.
//
// Tokens are the on-the-wire/stored identifiers; multiword ones are hyphenated
// so they read cleanly as a URL path segment and an admin-UI chip.
const (
	CapSend          = "send"           // POST /bots/{id}/send — post a message as the member
	CapDelete        = "delete"         // POST /bots/{id}/delete — soft-delete a chat message
	CapBan           = "ban"            // POST /bots/{id}/ban — ban a member
	CapRename        = "rename"         // POST /bots/{id}/rename — rename a channel
	CapForward       = "forward"        // POST /bots/{id}/forward — forward a message to another channel
	CapPromote       = "promote"        // POST /bots/{id}/promote — promote a message to a forum thread
	CapCreateChannel = "create-channel" // POST /bots/{id}/create-channel — create a channel
	CapSetTopic      = "set-topic"      // POST /bots/{id}/set-topic — set a channel's topic
	CapArchive       = "archive"        // POST /bots/{id}/archive — archive a channel
	CapDeleteChannel = "delete-channel" // POST /bots/{id}/delete-channel — delete a channel (destructive)
	CapBookmark      = "bookmark"       // POST /bots/{id}/bookmark — bookmark a message (the member's own list)
	CapTodo          = "todo"           // POST /bots/{id}/todo — add a message to the member's to-dos
	CapDM            = "dm"             // POST /bots/{id}/dm — open/append a direct-message thread to a member
)

// KnownCapabilities is the set the admin UI offers and the service validates
// against, so a typo or an injected unknown token is dropped, not granted. The
// order here is the order the checkboxes render in (grouped by capGroup).
var KnownCapabilities = []string{
	CapSend, CapForward, CapPromote, CapDelete, // messaging
	CapBan, // members
	CapRename, CapSetTopic, CapArchive, CapCreateChannel, CapDeleteChannel, // channels
	CapBookmark, CapTodo, CapDM, // personal
}

// validCapability reports whether cap is one the admin may grant.
func validCapability(cap string) bool {
	for _, k := range KnownCapabilities {
		if k == cap {
			return true
		}
	}
	return false
}

// Connector is one external-chat-bot integration. Secret is the per-connector
// HMAC key — never rendered to a non-admin and revealed to the operator only on
// create/rotate. UserID is the synthetic member the connector posts as.
type Connector struct {
	ID           string
	CommunityID  string
	UserID       string // synthetic member identity; sends are authored by this user
	Name         string // nick == membership display name
	AvatarURL    string
	Secret       string   // HMAC key for the stream signature and the /send X-Signature
	Capabilities []string // moderation powers the admin granted (always includes CapSend unless cleared)
	MentionsOnly bool     // stream filter: deliver only messages that @mention this connector
	Enabled      bool
	CreatedBy    string
	CreatedAt    time.Time
	LastSeenAt   *time.Time
	LastStatus   string

	// CursorAt is the server-owned resume watermark (the furthest message second
	// delivered to this connector's stream). nil = no position yet → first connect
	// is live-only; a zero/epoch value = an admin "Reset replay" → next connect
	// replays the whole catch-up window. See stream.go resumeWatermark + §migration
	// 00074. Written once on stream close, never per message.
	CursorAt *time.Time
}

// Can reports whether the connector was granted a capability — the single
// authority every signed action endpoint checks before acting.
func (c Connector) Can(cap string) bool {
	for _, g := range c.Capabilities {
		if g == cap {
			return true
		}
	}
	return false
}

// normalizeCapabilities lower-cases, de-dups, drops unknown tokens, and sorts a
// requested grant set so storage is canonical and an injected/garbage token can
// never become a granted power.
func normalizeCapabilities(caps []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range caps {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] || !validCapability(c) {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// joinCapabilities / splitCapabilities convert the set to/from the stored CSV.
func joinCapabilities(caps []string) string { return strings.Join(caps, ",") }
func splitCapabilities(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

// Repo is the SQL boundary for connectors. Box (optional) seals/opens the
// per-connector HMAC Secret at rest (FIX1 M20); nil (tests) stores bare. Log
// (optional) surfaces a secret-open failure so a key rotation/mismatch that
// silently breaks a connector's HMAC auth is at least diagnosable.
type Repo struct {
	DB  *sql.DB
	Box *secretbox.Box
	Log *slog.Logger
}

// NewRepo returns a Repo bound to db.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// openSecret decrypts a stored connector secret (FIX1 M20). Open accepts bare
// legacy plaintext, so pre-seal rows keep working; nil Box is a no-op.
func (r *Repo) openSecret(c *Connector) {
	if r.Box == nil {
		return
	}
	sec, err := r.Box.Open(c.Secret)
	if err != nil {
		// Fail closed (HMAC auth will reject), but make the cause visible — a
		// SECRETS_KEY rotation/mismatch would otherwise break the connector
		// silently (code-review follow-up to M20).
		if r.Log != nil {
			r.Log.Warn("connectors: open secret failed", "connector", c.ID, "err", err)
		}
		return
	}
	c.Secret = sec
}

// sealSecret encrypts a connector secret for storage (FIX1 M20). nil Box returns
// the input unchanged.
func (r *Repo) sealSecret(secret string) (string, error) {
	if r.Box == nil {
		return secret, nil
	}
	return r.Box.Seal(secret)
}

const selectCols = `id, community_id, user_id, name, avatar_url, secret, capabilities,
	mentions_only, enabled, COALESCE(created_by, ''), created_at, last_seen_at, last_status, cursor_at`

func scanConnector(s interface{ Scan(...any) error }) (Connector, error) {
	var c Connector
	var capabilities string
	var mentionsOnly, enabled int
	var created int64
	var lastSeen, cursor sql.NullInt64
	if err := s.Scan(&c.ID, &c.CommunityID, &c.UserID, &c.Name, &c.AvatarURL, &c.Secret, &capabilities,
		&mentionsOnly, &enabled, &c.CreatedBy, &created, &lastSeen, &c.LastStatus, &cursor); err != nil {
		return Connector{}, err
	}
	c.Capabilities = splitCapabilities(capabilities)
	c.MentionsOnly = mentionsOnly != 0
	c.Enabled = enabled != 0
	c.CreatedAt = time.Unix(created, 0)
	if lastSeen.Valid {
		t := time.Unix(lastSeen.Int64, 0)
		c.LastSeenAt = &t
	}
	// cursor_at is NULL until the first stream closes; a NULL CursorAt means
	// "live-only first connect" (resumeWatermark), distinct from a stored 0/epoch
	// ("reset → replay the window").
	if cursor.Valid {
		t := time.Unix(cursor.Int64, 0)
		c.CursorAt = &t
	}
	return c, nil
}

// ByID returns the enabled connector for id, or ErrNotFound. The public stream
// and send endpoints use this — a miss must look identical to a non-existent
// URL (anti-enumeration), so callers map ErrNotFound to a 404.
func (r *Repo) ByID(ctx context.Context, id string) (Connector, error) {
	if id == "" {
		return Connector{}, ErrNotFound
	}
	row := r.DB.QueryRowContext(ctx, `SELECT `+selectCols+`
		FROM connectors WHERE id = ? AND enabled = 1`, id)
	c, err := scanConnector(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, ErrNotFound
	}
	r.openSecret(&c)
	return c, err
}

// byIDInCommunity returns a connector scoped to its community REGARDLESS of the
// enabled flag — the admin/service path (edit, delete, rotate) must reach a
// disabled connector too. The public stream/send path uses ByID (enabled only).
func (r *Repo) byIDInCommunity(ctx context.Context, communityID, id string) (Connector, error) {
	row := r.DB.QueryRowContext(ctx, `SELECT `+selectCols+`
		FROM connectors WHERE id = ? AND community_id = ?`, id, communityID)
	c, err := scanConnector(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, ErrNotFound
	}
	r.openSecret(&c)
	return c, err
}

// ListForCommunity returns every connector in a community, newest last.
func (r *Repo) ListForCommunity(ctx context.Context, communityID string) ([]Connector, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+selectCols+`
		FROM connectors WHERE community_id = ? ORDER BY created_at`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		r.openSecret(&c)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Create inserts a connector row. The synthetic member (user_id) must already
// exist — the service provisions it first so the FK holds.
func (r *Repo) Create(ctx context.Context, c Connector) error {
	secret, err := r.sealSecret(c.Secret) // FIX1 M20: seal HMAC secret at rest
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
		INSERT INTO connectors (id, community_id, user_id, name, avatar_url, secret, capabilities,
			mentions_only, enabled, created_by, created_at, last_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		c.ID, c.CommunityID, c.UserID, c.Name, c.AvatarURL, secret, joinCapabilities(c.Capabilities),
		boolToInt(c.MentionsOnly), boolToInt(c.Enabled), nullable(c.CreatedBy), c.CreatedAt.Unix())
	return err
}

// SetCapabilities replaces a connector's granted capability set, scoped to its
// community. The caller normalizes the set first (validates + de-dups).
func (r *Repo) SetCapabilities(ctx context.Context, communityID, id string, caps []string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE connectors SET capabilities = ? WHERE id = ? AND community_id = ?`,
		joinCapabilities(caps), id, communityID)
	return err
}

// SetEnabled flips a connector's enabled flag, scoped to its community.
func (r *Repo) SetEnabled(ctx context.Context, communityID, id string, enabled bool) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE connectors SET enabled = ? WHERE id = ? AND community_id = ?`,
		boolToInt(enabled), id, communityID)
	return err
}

// SetMeta updates the connector's display fields (name + avatar + mentions_only),
// scoped to its community. The caller renames the synthetic member separately.
func (r *Repo) SetMeta(ctx context.Context, communityID, id, name, avatar string, mentionsOnly bool) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE connectors SET name = ?, avatar_url = ?, mentions_only = ? WHERE id = ? AND community_id = ?`,
		name, avatar, boolToInt(mentionsOnly), id, communityID)
	return err
}

// RotateSecret replaces a connector's HMAC secret, invalidating the old stream
// URL and every prior body signature at once.
func (r *Repo) RotateSecret(ctx context.Context, communityID, id, secret string) error {
	sealed, err := r.sealSecret(secret) // FIX1 M20: seal HMAC secret at rest
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx,
		`UPDATE connectors SET secret = ? WHERE id = ? AND community_id = ?`,
		sealed, id, communityID)
	return err
}

// Delete removes a connector scoped to its community. connector_channels
// CASCADEs; the synthetic member is removed by the service (no user_id cascade).
func (r *Repo) Delete(ctx context.Context, communityID, id string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM connectors WHERE id = ? AND community_id = ?`, id, communityID)
	return err
}

// Stamp records the last stream-connect / send time and a short status.
func (r *Repo) Stamp(ctx context.Context, id, status string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE connectors SET last_seen_at = ?, last_status = ? WHERE id = ?`,
		time.Now().Unix(), status, id)
	return err
}

// SetCursor stores the resume watermark for a connector, scoped to its community
// (defence-in-depth even though both callers already trust the id). The stream
// calls it ONCE on close with the furthest delivered second so a reconnect
// resumes there; the admin "Reset replay" calls it with 0 so the next connect
// replays the whole catch-up window. One write per connection, never per message
// (§8 single-writer).
func (r *Repo) SetCursor(ctx context.Context, communityID, id string, unix int64) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE connectors SET cursor_at = ? WHERE id = ? AND community_id = ?`,
		unix, id, communityID)
	return err
}

// SetChannels replaces a connector's channel allowlist in one transaction:
// delete all links, then insert the given channel ids. An empty slice clears the
// allowlist (= all channels). channelIDs are validated by the service to belong
// to the connector's community before this is called.
func (r *Repo) SetChannels(ctx context.Context, connectorID string, channelIDs []string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM connector_channels WHERE connector_id = ?`, connectorID); err != nil {
		return err
	}
	for _, ch := range channelIDs {
		if ch == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO connector_channels (connector_id, channel_id) VALUES (?, ?)
			ON CONFLICT DO NOTHING`, connectorID, ch); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Channels returns the connector's allowlisted channel ids (possibly empty).
func (r *Repo) Channels(ctx context.Context, connectorID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT channel_id FROM connector_channels WHERE connector_id = ?`, connectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
