package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Repo is the SQLite-backed persistence layer for the mailbox feature.
type Repo struct {
	DB *sql.DB

	// filterCache holds the active filter set. Read on every polled
	// message; written by ListFilters on miss + InvalidateFilters when
	// a CRUD handler touches a row. Guarded by mu.
	mu          sync.RWMutex
	filterCache []Filter
	filterFresh bool
}

// NewRepo wraps a connection.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// InvalidateFilters drops the in-memory filter cache. Phase 8's filter
// CRUD handlers must call this after every mutation.
func (r *Repo) InvalidateFilters() {
	r.mu.Lock()
	r.filterFresh = false
	r.filterCache = nil
	r.mu.Unlock()
}

// cachedFilters returns the current set, repopulating from SQL on miss.
// Sub-millisecond on hot path because we hold the slice in memory.
func (r *Repo) cachedFilters(ctx context.Context) ([]Filter, error) {
	r.mu.RLock()
	if r.filterFresh {
		out := r.filterCache
		r.mu.RUnlock()
		return out, nil
	}
	r.mu.RUnlock()

	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, community_id, kind, pattern, to_issue, created_by, created_at
		FROM community_mail_filter`)
	if err != nil {
		return nil, fmt.Errorf("filter list: %w", err)
	}
	defer rows.Close()
	out := []Filter{}
	for rows.Next() {
		var f Filter
		var kind string
		var toIssue int
		var created int64
		if err := rows.Scan(&f.ID, &f.CommunityID, &kind, &f.Pattern, &toIssue, &f.CreatedBy, &created); err != nil {
			return nil, err
		}
		f.Kind = FilterKind(kind)
		f.ToIssue = toIssue != 0
		f.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.filterCache = out
	r.filterFresh = true
	r.mu.Unlock()
	return out, nil
}

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

// UpsertFolder records that the worker observed a folder during the
// cycle. If UIDVALIDITY has changed since the last record, last_uid is
// reset to 0 — the next cycle re-scans the folder from scratch.
// Returns the persisted folder (post-update), including the canonical
// last_uid value the caller should advance from.
func (r *Repo) UpsertFolder(ctx context.Context, accountID, name string, uidvalidity uint32) (Folder, error) {
	if name == "" {
		return Folder{}, errors.New("mailbox: folder name required")
	}
	now := time.Now().Unix()
	existing, err := r.folderByName(ctx, accountID, name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Folder{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		f := Folder{
			ID:          uuid.NewString(),
			AccountID:   accountID,
			Name:        name,
			UIDValidity: uidvalidity,
			LastUID:     0,
			Enabled:     true,
		}
		if _, err := r.DB.ExecContext(ctx, `
			INSERT INTO mailbox_folder (id, account_id, name, uidvalidity, last_uid, enabled, last_seen_at)
			VALUES (?, ?, ?, ?, 0, 1, ?)`,
			f.ID, accountID, name, uidvalidity, now); err != nil {
			return Folder{}, fmt.Errorf("insert mailbox_folder: %w", err)
		}
		t := time.Unix(now, 0).UTC()
		f.LastSeenAt = &t
		return f, nil
	}
	// UIDVALIDITY rotation — server's mailbox identity changed. Reset
	// the cursor so the next FETCH treats every UID as new.
	if existing.UIDValidity != uidvalidity {
		if _, err := r.DB.ExecContext(ctx, `
			UPDATE mailbox_folder
			SET uidvalidity = ?, last_uid = 0, last_seen_at = ?
			WHERE id = ?`, uidvalidity, now, existing.ID); err != nil {
			return Folder{}, fmt.Errorf("rotate mailbox_folder uidvalidity: %w", err)
		}
		existing.UIDValidity = uidvalidity
		existing.LastUID = 0
		t := time.Unix(now, 0).UTC()
		existing.LastSeenAt = &t
		return existing, nil
	}
	if _, err := r.DB.ExecContext(ctx, `
		UPDATE mailbox_folder SET last_seen_at = ? WHERE id = ?`,
		now, existing.ID); err != nil {
		return Folder{}, err
	}
	t := time.Unix(now, 0).UTC()
	existing.LastSeenAt = &t
	return existing, nil
}

// SetFolderLastUID advances the per-folder cursor after a successful
// batch. The caller passes the max UID it consumed in the cycle.
func (r *Repo) SetFolderLastUID(ctx context.Context, folderID string, lastUID uint32) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE mailbox_folder SET last_uid = ? WHERE id = ? AND last_uid < ?`,
		lastUID, folderID, lastUID)
	return err
}

func (r *Repo) folderByName(ctx context.Context, accountID, name string) (Folder, error) {
	var f Folder
	var lastSeen sql.NullInt64
	var lastErr sql.NullString
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, account_id, name, uidvalidity, last_uid, enabled, last_seen_at, last_error
		FROM mailbox_folder
		WHERE account_id = ? AND name = ?`, accountID, name).
		Scan(&f.ID, &f.AccountID, &f.Name, &f.UIDValidity, &f.LastUID, &f.Enabled, &lastSeen, &lastErr)
	if err != nil {
		return Folder{}, err
	}
	if lastSeen.Valid {
		t := time.Unix(lastSeen.Int64, 0).UTC()
		f.LastSeenAt = &t
	}
	if lastErr.Valid {
		f.LastError = lastErr.String
	}
	return f, nil
}

// IngestInsert captures every field needed to persist one matched
// envelope into email_ingest. The caller (poll loop) populates this
// from the FetchedEnvelope + matched Filter.
type IngestInsert struct {
	FolderID        string
	UID             uint32
	UIDValidity     uint32
	MessageID       string
	FromAddr        string
	FromName        string
	Subject         string
	ReceivedAt      time.Time
	CommunityID     string
	MatchedFilterID string
}

// InsertIngest persists one matched email. The unique constraint on
// (folder_id, uid, uidvalidity) absorbs duplicates from re-runs, and
// the second return value tells the caller whether the row is brand-new
// so side-effects (auto-issue creation, broadcasts) only fire once.
func (r *Repo) InsertIngest(ctx context.Context, in IngestInsert) (id string, isNew bool, err error) {
	if existing, err := r.findIngestUID(ctx, in.FolderID, in.UID, in.UIDValidity); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	id = uuid.NewString()
	now := time.Now().Unix()
	receivedMS := in.ReceivedAt.UnixMilli()
	_, err = r.DB.ExecContext(ctx, `
		INSERT INTO email_ingest (
			id, folder_id, uid, uidvalidity, message_id,
			from_addr, from_name, subject, received_at,
			community_id, status, matched_filter_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		id, in.FolderID, in.UID, in.UIDValidity, in.MessageID,
		strings.ToLower(in.FromAddr), in.FromName, in.Subject, receivedMS,
		in.CommunityID, nullIfEmpty(in.MatchedFilterID), now,
	)
	if err != nil {
		return "", false, fmt.Errorf("insert email_ingest: %w", err)
	}
	return id, true, nil
}

func (r *Repo) findIngestUID(ctx context.Context, folderID string, uid, uidvalidity uint32) (string, error) {
	var id string
	err := r.DB.QueryRowContext(ctx, `
		SELECT id FROM email_ingest
		WHERE folder_id = ? AND uid = ? AND uidvalidity = ?`,
		folderID, uid, uidvalidity).Scan(&id)
	return id, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
