package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Repo is the SQLite-backed persistence layer for the mailbox feature.
type Repo struct{ DB *sql.DB }

// NewRepo wraps a connection.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// AccountConfig captures the fields needed to EnsureAccount from env.
type AccountConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	TLSMode  string
}

// EnsureAccount inserts the singleton mailbox_account row on first boot
// and updates it if any field changed (host/port/user/pass/tls). Returns
// the persisted row. mailbox_account is keyed by id, not by host+user, so
// the singleton invariant is enforced here in code.
func (r *Repo) EnsureAccount(ctx context.Context, cfg AccountConfig) (Account, error) {
	if cfg.Host == "" || cfg.Username == "" {
		return Account{}, errors.New("mailbox: host and username required")
	}
	existing, err := r.findFirstAccount(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}
	now := time.Now().UTC()
	if errors.Is(err, sql.ErrNoRows) {
		acc := Account{
			ID:        uuid.NewString(),
			Host:      cfg.Host,
			Port:      cfg.Port,
			Username:  cfg.Username,
			Password:  cfg.Password,
			TLSMode:   cfg.TLSMode,
			CreatedAt: now,
		}
		_, err := r.DB.ExecContext(ctx, `
			INSERT INTO mailbox_account (id, host, port, username, password, tls_mode, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			acc.ID, acc.Host, acc.Port, acc.Username, acc.Password, acc.TLSMode, acc.CreatedAt.Unix())
		if err != nil {
			return Account{}, fmt.Errorf("insert mailbox_account: %w", err)
		}
		return acc, nil
	}
	if existing.Host != cfg.Host || existing.Port != cfg.Port ||
		existing.Username != cfg.Username || existing.Password != cfg.Password ||
		existing.TLSMode != cfg.TLSMode {
		if _, err := r.DB.ExecContext(ctx, `
			UPDATE mailbox_account
			SET host = ?, port = ?, username = ?, password = ?, tls_mode = ?
			WHERE id = ?`,
			cfg.Host, cfg.Port, cfg.Username, cfg.Password, cfg.TLSMode, existing.ID); err != nil {
			return Account{}, fmt.Errorf("update mailbox_account: %w", err)
		}
		existing.Host = cfg.Host
		existing.Port = cfg.Port
		existing.Username = cfg.Username
		existing.Password = cfg.Password
		existing.TLSMode = cfg.TLSMode
	}
	return existing, nil
}

func (r *Repo) findFirstAccount(ctx context.Context) (Account, error) {
	var acc Account
	var lastPoll sql.NullInt64
	var lastErr sql.NullString
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, host, port, username, password, tls_mode, last_poll_at, last_error, created_at
		FROM mailbox_account
		ORDER BY created_at
		LIMIT 1`).
		Scan(&acc.ID, &acc.Host, &acc.Port, &acc.Username, &acc.Password, &acc.TLSMode,
			&lastPoll, &lastErr, &created)
	if err != nil {
		return Account{}, err
	}
	if lastPoll.Valid {
		t := time.Unix(lastPoll.Int64, 0).UTC()
		acc.LastPollAt = &t
	}
	if lastErr.Valid {
		acc.LastError = lastErr.String
	}
	acc.CreatedAt = time.Unix(created, 0).UTC()
	return acc, nil
}

// ListEnabledFolders returns folders the poll worker should examine on
// the next cycle. Phase 1 returns an empty set since no folders have
// been discovered yet — Phase 2 populates rows via UpsertFolder.
func (r *Repo) ListEnabledFolders(ctx context.Context, accountID string) ([]Folder, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, account_id, name, uidvalidity, last_uid, enabled, last_seen_at, last_error
		FROM mailbox_folder
		WHERE account_id = ? AND enabled = 1
		ORDER BY name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		var f Folder
		var lastSeen sql.NullInt64
		var lastErr sql.NullString
		if err := rows.Scan(&f.ID, &f.AccountID, &f.Name, &f.UIDValidity, &f.LastUID,
			&f.Enabled, &lastSeen, &lastErr); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			t := time.Unix(lastSeen.Int64, 0).UTC()
			f.LastSeenAt = &t
		}
		if lastErr.Valid {
			f.LastError = lastErr.String
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// QueuedEmailView is the row shape consumed by the inbox template. The
// Attachments slice is preloaded so the template iterates without
// extra queries.
type QueuedEmailView struct {
	ID          string
	CommunityID string
	FromAddr    string
	FromName    string
	Subject     string
	ReceivedAt  time.Time
	Status      IngestStatus
	Attachments []QueuedAttachmentView
}

// QueuedAttachmentView is one attachment row inside QueuedEmailView.
type QueuedAttachmentView struct {
	ID           string
	Filename     string
	MIME         string
	SizeBytes    int64
	IsMaterialised bool
}

// QueueForViewer returns up to q.Limit ingest rows the viewer is allowed
// to see — limited to communities they're admin/mod in and optionally
// narrowed to one community pill. Phase 1 implements the query but the
// db will be empty until Phase 3 starts persisting matches; the empty
// slice is the expected Phase 1 result.
//
// The next-page cursor is returned when there is more to fetch; nil
// means the viewer has paged through the whole queue.
func (r *Repo) QueueForViewer(ctx context.Context, q QueueQuery) ([]QueuedEmailView, *QueueCursor, error) {
	if len(q.AdminCommunityIDs) == 0 {
		return []QueuedEmailView{}, nil, nil
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}

	var (
		args  []any
		where = []string{"i.status = 'queued'"}
	)

	if q.CommunityFilter != "" {
		where = append(where, "i.community_id = ?")
		args = append(args, q.CommunityFilter)
	} else {
		placeholders := strings.Repeat("?,", len(q.AdminCommunityIDs))
		placeholders = placeholders[:len(placeholders)-1]
		where = append(where, "i.community_id IN ("+placeholders+")")
		for _, cid := range q.AdminCommunityIDs {
			args = append(args, cid)
		}
	}

	if q.Cursor != nil {
		where = append(where, "(i.received_at < ? OR (i.received_at = ? AND i.id < ?))")
		args = append(args, q.Cursor.ReceivedAtUnixMS, q.Cursor.ReceivedAtUnixMS, q.Cursor.ID)
	}

	args = append(args, q.Limit+1) // fetch one extra to detect "has more"

	query := fmt.Sprintf(`
		SELECT i.id, i.community_id, i.from_addr, i.from_name, i.subject,
		       i.received_at, i.status
		FROM email_ingest i
		WHERE %s
		ORDER BY i.received_at DESC, i.id DESC
		LIMIT ?`, strings.Join(where, " AND "))

	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("queue list: %w", err)
	}
	defer rows.Close()

	out := []QueuedEmailView{}
	for rows.Next() {
		var v QueuedEmailView
		var received int64
		var status string
		if err := rows.Scan(&v.ID, &v.CommunityID, &v.FromAddr, &v.FromName, &v.Subject,
			&received, &status); err != nil {
			return nil, nil, err
		}
		v.ReceivedAt = time.UnixMilli(received).UTC()
		v.Status = IngestStatus(status)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var next *QueueCursor
	if len(out) > q.Limit {
		last := out[q.Limit-1]
		next = &QueueCursor{
			ReceivedAtUnixMS: last.ReceivedAt.UnixMilli(),
			ID:               last.ID,
		}
		out = out[:q.Limit]
	}

	if len(out) > 0 {
		if err := r.attachAttachmentsBulk(ctx, out); err != nil {
			return nil, nil, err
		}
	}

	return out, next, nil
}

// attachAttachmentsBulk loads attachment rows for every ingest in the
// page in one round-trip and slots them onto the view structs in place.
func (r *Repo) attachAttachmentsBulk(ctx context.Context, views []QueuedEmailView) error {
	if len(views) == 0 {
		return nil
	}
	byID := make(map[string]int, len(views))
	args := make([]any, 0, len(views))
	for i, v := range views {
		byID[v.ID] = i
		args = append(args, v.ID)
	}
	placeholders := strings.Repeat("?,", len(views))
	placeholders = placeholders[:len(placeholders)-1]
	rows, err := r.DB.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, ingest_id, filename, mime, size_bytes, upload_id IS NOT NULL
		FROM email_ingest_attachment
		WHERE ingest_id IN (%s)
		ORDER BY filename`, placeholders), args...)
	if err != nil {
		return fmt.Errorf("attachments bulk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a QueuedAttachmentView
		var ingestID string
		var materialised int
		if err := rows.Scan(&a.ID, &ingestID, &a.Filename, &a.MIME, &a.SizeBytes, &materialised); err != nil {
			return err
		}
		a.IsMaterialised = materialised != 0
		if idx, ok := byID[ingestID]; ok {
			views[idx].Attachments = append(views[idx].Attachments, a)
		}
	}
	return rows.Err()
}
