// Package dataexport builds an owner-initiated ZIP of ALL of a community's data
// and media, downloadable via a 7-day signed (capability-token) URL. It is the
// portability counterpart to the community-delete + account-erasure seams: a
// SaaS tenant owner can take their data with them.
//
// Platform property is excluded by design — agent system prompts, RAG vectors,
// and (by a column-name rule) every secret. See eidos/spec - data-export.
package dataexport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status values for a community_exports row.
const (
	StatusPending  = "pending"
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusFailed   = "failed"
	StatusExpired  = "expired"
)

// TTL is how long a built export stays downloadable before the sweep deletes it.
const TTL = 7 * 24 * time.Hour

// ErrInProgress is returned by Request when an export is already pending/building
// for the community — one active build at a time.
var ErrInProgress = errors.New("dataexport: an export is already in progress")

// Export is one community_exports row.
type Export struct {
	ID          string
	CommunityID string
	RequestedBy string
	Status      string
	Token       string
	RelPath     string
	SizeBytes   int64
	Error       string
	RequestedAt time.Time
	ReadyAt     *time.Time
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// IsDownloadable reports whether the export can be served right now.
func (e Export) IsDownloadable(now time.Time) bool {
	return e.Status == StatusReady && e.ExpiresAt != nil && now.Before(*e.ExpiresAt)
}

// Repo is the community_exports data access. Stateless; all SQL lives here.
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// Request inserts a fresh pending export for the community, refusing if one is
// already pending or building. The returned Export has its id set; the worker
// fills in token/path/expiry on completion.
func (r *Repo) Request(ctx context.Context, communityID, requestedBy string) (Export, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM community_exports WHERE community_id = ? AND status IN (?, ?)`,
		communityID, StatusPending, StatusBuilding).Scan(&n); err != nil {
		return Export{}, err
	}
	if n > 0 {
		return Export{}, ErrInProgress
	}
	e := Export{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		RequestedBy: requestedBy,
		Status:      StatusPending,
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO community_exports (id, community_id, requested_by, status, requested_at, created_at)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, ?)`,
		e.ID, e.CommunityID, e.RequestedBy, e.Status, e.RequestedAt.Unix(), e.CreatedAt.Unix()); err != nil {
		return Export{}, err
	}
	return e, nil
}

const exportCols = `id, community_id, COALESCE(requested_by,''), status, token, rel_path, size_bytes, error, requested_at, ready_at, expires_at, created_at`

func scanExport(row interface{ Scan(...any) error }) (Export, error) {
	var e Export
	var reqAt, createdAt int64
	var readyAt, expiresAt sql.NullInt64
	if err := row.Scan(&e.ID, &e.CommunityID, &e.RequestedBy, &e.Status, &e.Token,
		&e.RelPath, &e.SizeBytes, &e.Error, &reqAt, &readyAt, &expiresAt, &createdAt); err != nil {
		return Export{}, err
	}
	e.RequestedAt = time.Unix(reqAt, 0)
	e.CreatedAt = time.Unix(createdAt, 0)
	if readyAt.Valid {
		t := time.Unix(readyAt.Int64, 0)
		e.ReadyAt = &t
	}
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0)
		e.ExpiresAt = &t
	}
	return e, nil
}

// Get fetches one export by id. Returns sql.ErrNoRows when absent.
func (r *Repo) Get(ctx context.Context, id string) (Export, error) {
	return scanExport(r.DB.QueryRowContext(ctx,
		`SELECT `+exportCols+` FROM community_exports WHERE id = ?`, id))
}

// Latest returns the most recent export for the community, or (zero, false) when
// the community has never requested one.
func (r *Repo) Latest(ctx context.Context, communityID string) (Export, bool, error) {
	e, err := scanExport(r.DB.QueryRowContext(ctx,
		`SELECT `+exportCols+` FROM community_exports WHERE community_id = ? ORDER BY created_at DESC LIMIT 1`,
		communityID))
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, false, nil
	}
	if err != nil {
		return Export{}, false, err
	}
	return e, true, nil
}

// NextPending claims the oldest pending export for the worker, flipping it to
// building in the same statement so two workers can't grab the same row. Returns
// (zero, false) when the queue is empty.
func (r *Repo) NextPending(ctx context.Context) (Export, bool, error) {
	var id string
	err := r.DB.QueryRowContext(ctx,
		`SELECT id FROM community_exports WHERE status = ? ORDER BY requested_at LIMIT 1`,
		StatusPending).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, false, nil
	}
	if err != nil {
		return Export{}, false, err
	}
	res, err := r.DB.ExecContext(ctx,
		`UPDATE community_exports SET status = ? WHERE id = ? AND status = ?`,
		StatusBuilding, id, StatusPending)
	if err != nil {
		return Export{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Export{}, false, nil // raced; let the next tick retry
	}
	e, err := r.Get(ctx, id)
	if err != nil {
		return Export{}, false, err
	}
	return e, true, nil
}

// MarkReady stamps the finished artifact: token, path, size, and a TTL expiry.
func (r *Repo) MarkReady(ctx context.Context, id, token, relPath string, size int64) error {
	now := time.Now()
	_, err := r.DB.ExecContext(ctx, `
		UPDATE community_exports
		SET status = ?, token = ?, rel_path = ?, size_bytes = ?, error = '', ready_at = ?, expires_at = ?
		WHERE id = ?`,
		StatusReady, token, relPath, size, now.Unix(), now.Add(TTL).Unix(), id)
	return err
}

// MarkFailed records a build error.
func (r *Repo) MarkFailed(ctx context.Context, id, msg string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE community_exports SET status = ?, error = ? WHERE id = ?`, StatusFailed, msg, id)
	return err
}

// MarkExpired flips a ready export to expired (after its file is removed).
func (r *Repo) MarkExpired(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE community_exports SET status = ?, rel_path = '', token = '' WHERE id = ?`, StatusExpired, id)
	return err
}

// ListExpirable returns ready exports whose expiry has passed (file still on
// disk), for the sweep to delete.
func (r *Repo) ListExpirable(ctx context.Context, now time.Time) ([]Export, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT `+exportCols+` FROM community_exports WHERE status = ? AND expires_at IS NOT NULL AND expires_at < ?`,
		StatusReady, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Export
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// newToken returns a 32-byte high-entropy capability token (hex). This is the
// bearer secret in the download URL — the same pattern as the portal module's
// 7-day links.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dataexport: token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
