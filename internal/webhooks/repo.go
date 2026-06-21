// Package webhooks provides per-community inbound and outbound webhook
// integrations. Inbound: external systems POST to /hooks/<token> and the
// payload is parsed by a provider adapter into a bot chat message. Outbound:
// new human chat messages in a chosen channel are relayed as a JSON POST to an
// external URL. It is the push-driven mirror of internal/mailbox.
package webhooks

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Direction values for a webhook row.
const (
	DirIn  = "in"
	DirOut = "out"
)

// ErrNotFound is returned when no enabled webhook matches a lookup.
var ErrNotFound = errors.New("webhooks: not found")

// Webhook is one inbound or outbound integration.
type Webhook struct {
	ID          string
	CommunityID string
	Direction   string // in | out
	Provider    string // in: generic|github ; out: slack|discord|generic
	Name        string // bot display name (in) / label (out)
	AvatarURL   string
	ChannelID   string // in: target channel ; out: source filter ("" = all channels)
	Token       string // in: URL secret
	Secret      string // in: optional HMAC signing secret (github)
	TargetURL   string // out: POST destination
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
	LastAt      *time.Time
	LastStatus  string
}

// Repo is the SQL boundary for webhooks. Stateless; all state is in *sql.DB.
type Repo struct{ DB *sql.DB }

// NewRepo returns a Repo bound to db.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

const selectCols = `id, community_id, direction, provider, name, avatar_url,
	COALESCE(channel_id, ''), token, secret, target_url, enabled,
	COALESCE(created_by, ''), created_at, last_at, last_status`

func scanWebhook(s interface{ Scan(...any) error }) (Webhook, error) {
	var w Webhook
	var enabled int
	var created int64
	var lastAt sql.NullInt64
	if err := s.Scan(&w.ID, &w.CommunityID, &w.Direction, &w.Provider, &w.Name, &w.AvatarURL,
		&w.ChannelID, &w.Token, &w.Secret, &w.TargetURL, &enabled,
		&w.CreatedBy, &created, &lastAt, &w.LastStatus); err != nil {
		return Webhook{}, err
	}
	w.Enabled = enabled != 0
	w.CreatedAt = time.Unix(created, 0)
	if lastAt.Valid {
		t := time.Unix(lastAt.Int64, 0)
		w.LastAt = &t
	}
	return w, nil
}

// InboundByToken returns the enabled inbound webhook for token, or ErrNotFound.
func (r *Repo) InboundByToken(ctx context.Context, token string) (Webhook, error) {
	if token == "" {
		return Webhook{}, ErrNotFound
	}
	row := r.DB.QueryRowContext(ctx, `SELECT `+selectCols+`
		FROM webhooks WHERE token = ? AND direction = 'in' AND enabled = 1`, token)
	w, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	return w, err
}

// ListForCommunity returns every webhook in a community, inbound first, newest last.
func (r *Repo) ListForCommunity(ctx context.Context, communityID string) ([]Webhook, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+selectCols+`
		FROM webhooks WHERE community_id = ?
		ORDER BY direction, created_at`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// OutboundForChannel returns enabled outbound webhooks whose channel filter is
// NULL (all channels) or equals channelID.
func (r *Repo) OutboundForChannel(ctx context.Context, communityID, channelID string) ([]Webhook, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+selectCols+`
		FROM webhooks
		WHERE community_id = ? AND direction = 'out' AND enabled = 1
		  AND (channel_id IS NULL OR channel_id = ?)`, communityID, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Create inserts a webhook. channel_id "" is stored as NULL.
func (r *Repo) Create(ctx context.Context, w Webhook) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO webhooks (id, community_id, direction, provider, name, avatar_url,
			channel_id, token, secret, target_url, enabled, created_by, created_at, last_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		w.ID, w.CommunityID, w.Direction, w.Provider, w.Name, w.AvatarURL,
		nullable(w.ChannelID), w.Token, w.Secret, w.TargetURL, boolToInt(w.Enabled),
		nullable(w.CreatedBy), w.CreatedAt.Unix())
	return err
}

// SetEnabled flips a webhook's enabled flag, scoped to its community.
func (r *Repo) SetEnabled(ctx context.Context, communityID, id string, enabled bool) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE webhooks SET enabled = ? WHERE id = ? AND community_id = ?`,
		boolToInt(enabled), id, communityID)
	return err
}

// RotateToken replaces an inbound webhook's token, invalidating the old URL.
func (r *Repo) RotateToken(ctx context.Context, communityID, id, token string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE webhooks SET token = ? WHERE id = ? AND community_id = ? AND direction = 'in'`,
		token, id, communityID)
	return err
}

// Delete removes a webhook scoped to its community.
func (r *Repo) Delete(ctx context.Context, communityID, id string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM webhooks WHERE id = ? AND community_id = ?`, id, communityID)
	return err
}

// Stamp records the last receipt/delivery time and status on a webhook.
func (r *Repo) Stamp(ctx context.Context, id, status string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE webhooks SET last_at = ?, last_status = ? WHERE id = ?`,
		time.Now().Unix(), status, id)
	return err
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
