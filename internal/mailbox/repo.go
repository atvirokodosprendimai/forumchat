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

// ResetAllFolderCursors zeroes every folder's last_uid for this account
// so the next poll cycle re-fetches every message. Invoked on boot when
// MAILBOX_RESCAN_ON_BOOT=true OR by the cli "mailbox rescan" command.
func (r *Repo) ResetAllFolderCursors(ctx context.Context, accountID string) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE mailbox_folder SET last_uid = 0 WHERE account_id = ?`, accountID)
	if err != nil {
		return 0, fmt.Errorf("reset folder cursors: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneSkippedFolderIngest hard-deletes email_ingest rows (and their
// FTS / attachment rows via CASCADE + manual FTS) that came from
// folders matching looksLikeSentOrTrash. Used to clean up rows that
// were ingested before the skip-folder fix landed.
//
// Returns the list of folder names pruned and the count of email_ingest
// rows deleted.
func (r *Repo) PruneSkippedFolderIngest(ctx context.Context, accountID string) ([]string, int64, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, name FROM mailbox_folder WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, 0, fmt.Errorf("list folders: %w", err)
	}
	var skipIDs []string
	var skipNames []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if looksLikeSentOrTrash(name) {
			skipIDs = append(skipIDs, id)
			skipNames = append(skipNames, name)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	if len(skipIDs) == 0 {
		return nil, 0, nil
	}

	placeholders := strings.Repeat("?,", len(skipIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(skipIDs))
	for i, id := range skipIDs {
		args[i] = id
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("prune tx begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM email_ingest_fts
		WHERE ingest_id IN (SELECT id FROM email_ingest WHERE folder_id IN (`+placeholders+`))`, args...); err != nil {
		return nil, 0, fmt.Errorf("prune fts: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM email_ingest WHERE folder_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("prune ingest: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("prune tx commit: %w", err)
	}
	return skipNames, n, nil
}

// WipeIngest hard-deletes every email_ingest + email_ingest_attachment
// + email_ingest_fts row, then resets the folder cursors so the next
// poll cycle starts from a clean slate. Used by the cli "mailbox wipe"
// command after the user has been pre-caching with no filters and
// wants the system to re-ingest into the new filter / global pile.
//
// Project attachments already materialised via "Move" stay put — they
// live in uploads + project_attachments, untouched here.
func (r *Repo) WipeIngest(ctx context.Context, accountID string) (int64, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("wipe tx begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var ingestCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_ingest`).Scan(&ingestCount); err != nil {
		return 0, fmt.Errorf("wipe count: %w", err)
	}
	// FK ON DELETE CASCADE handles attachments + ingest_issue link.
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_ingest`); err != nil {
		return 0, fmt.Errorf("wipe email_ingest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_ingest_fts`); err != nil {
		return 0, fmt.Errorf("wipe fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailbox_folder SET last_uid = 0 WHERE account_id = ?`, accountID); err != nil {
		return 0, fmt.Errorf("wipe folder cursors: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("wipe tx commit: %w", err)
	}
	return ingestCount, nil
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
	BodyText        string // text representation persisted for /inbox search
	ReceivedAt      time.Time
	CommunityID     string
	MatchedFilterID string
}

// InsertIngest persists one matched email. The unique constraint on
// (folder_id, uid, uidvalidity) absorbs same-folder duplicates from
// re-runs. We ALSO dedupe across folders by Message-ID — Gmail's
// [Gmail]/All Mail and the user's INBOX hold the same logical message
// with different UIDs, so without this guard every Gmail user would
// see every email twice. Empty Message-ID falls through (rare, not
// worth blocking ingest over).
//
// The second return value tells the caller whether the row is brand
// new so side-effects (auto-issue creation, broadcasts) only fire once.
func (r *Repo) InsertIngest(ctx context.Context, in IngestInsert) (id string, isNew bool, err error) {
	if existing, err := r.findIngestUID(ctx, in.FolderID, in.UID, in.UIDValidity); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if mid := strings.TrimSpace(in.MessageID); mid != "" {
		if existing, err := r.findIngestByMessageID(ctx, mid); err == nil {
			return existing, false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	id = uuid.NewString()
	now := time.Now().Unix()
	receivedMS := in.ReceivedAt.UnixMilli()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("ingest tx begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_ingest (
			id, folder_id, uid, uidvalidity, message_id,
			from_addr, from_name, subject, body_text, received_at,
			community_id, status, matched_filter_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		id, in.FolderID, in.UID, in.UIDValidity, in.MessageID,
		strings.ToLower(in.FromAddr), in.FromName, in.Subject, in.BodyText, receivedMS,
		nullIfEmpty(in.CommunityID), nullIfEmpty(in.MatchedFilterID), now,
	); err != nil {
		return "", false, fmt.Errorf("insert email_ingest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_ingest_fts (ingest_id, subject, from_addr, from_name, body_text, attachment_names)
		VALUES (?, ?, ?, ?, ?, '')`,
		id, in.Subject, strings.ToLower(in.FromAddr), in.FromName, in.BodyText,
	); err != nil {
		return "", false, fmt.Errorf("insert email_ingest_fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("ingest tx commit: %w", err)
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

// findIngestByMessageID returns the existing ingest id for a non-empty
// RFC 5322 Message-ID. The header is globally unique by convention so
// any other landing of the same logical email — typically Gmail's
// [Gmail]/All Mail mirroring INBOX — surfaces as a duplicate here.
func (r *Repo) findIngestByMessageID(ctx context.Context, messageID string) (string, error) {
	var id string
	err := r.DB.QueryRowContext(ctx, `
		SELECT id FROM email_ingest WHERE message_id = ? LIMIT 1`,
		messageID).Scan(&id)
	return id, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// InsertFilter persists a new community_mail_filter row and invalidates
// the in-memory filter cache so the next polled message sees the rule.
// It ALSO retro-assigns any unassigned email_ingest rows whose from_addr
// matches the new filter — so clicking "attach this sender to community X"
// pulls the visible queued mail from that sender into the chosen community
// instead of leaving it stuck in Unassigned.
func (r *Repo) InsertFilter(ctx context.Context, f Filter) (int64, error) {
	if f.ID == "" || f.CommunityID == "" || f.Pattern == "" || f.CreatedBy == "" {
		return 0, errors.New("mailbox: filter id/community/pattern/created_by required")
	}
	toIssue := 0
	if f.ToIssue {
		toIssue = 1
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO community_mail_filter
			(id, community_id, kind, pattern, to_issue, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.CommunityID, string(f.Kind), f.Pattern, toIssue, f.CreatedBy, time.Now().Unix(),
	); err != nil {
		return 0, fmt.Errorf("insert community_mail_filter: %w", err)
	}
	var backfill int64
	switch f.Kind {
	case FilterKindAddress:
		res, err := r.DB.ExecContext(ctx, `
			UPDATE email_ingest
			SET community_id = ?, matched_filter_id = ?
			WHERE community_id IS NULL AND from_addr = ?`,
			f.CommunityID, f.ID, f.Pattern)
		if err != nil {
			return 0, fmt.Errorf("backfill ingest by address: %w", err)
		}
		backfill, _ = res.RowsAffected()
	case FilterKindDomain:
		// pattern like "@example.com" — match suffix on from_addr.
		res, err := r.DB.ExecContext(ctx, `
			UPDATE email_ingest
			SET community_id = ?, matched_filter_id = ?
			WHERE community_id IS NULL AND from_addr LIKE ?`,
			f.CommunityID, f.ID, "%"+f.Pattern)
		if err != nil {
			return 0, fmt.Errorf("backfill ingest by domain: %w", err)
		}
		backfill, _ = res.RowsAffected()
	}
	r.InvalidateFilters()
	return backfill, nil
}

// BackfillIngestForFilter re-runs the unassigned-row backfill for an
// existing filter. Idempotent — only updates rows where community_id IS
// NULL, so already-routed mail is untouched. Returns the number of
// rows newly tagged.
func (r *Repo) BackfillIngestForFilter(ctx context.Context, f Filter) (int64, error) {
	switch f.Kind {
	case FilterKindAddress:
		res, err := r.DB.ExecContext(ctx, `
			UPDATE email_ingest
			SET community_id = ?, matched_filter_id = ?
			WHERE community_id IS NULL AND from_addr = ?`,
			f.CommunityID, f.ID, f.Pattern)
		if err != nil {
			return 0, fmt.Errorf("backfill ingest by address: %w", err)
		}
		n, _ := res.RowsAffected()
		return n, nil
	case FilterKindDomain:
		res, err := r.DB.ExecContext(ctx, `
			UPDATE email_ingest
			SET community_id = ?, matched_filter_id = ?
			WHERE community_id IS NULL AND from_addr LIKE ?`,
			f.CommunityID, f.ID, "%"+f.Pattern)
		if err != nil {
			return 0, fmt.Errorf("backfill ingest by domain: %w", err)
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	return 0, nil
}

// DeleteFilter removes one filter and invalidates the cache.
func (r *Repo) DeleteFilter(ctx context.Context, filterID, communityID string) error {
	res, err := r.DB.ExecContext(ctx, `
		DELETE FROM community_mail_filter WHERE id = ? AND community_id = ?`,
		filterID, communityID)
	if err != nil {
		return fmt.Errorf("delete community_mail_filter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("mailbox: filter not found in community")
	}
	r.InvalidateFilters()
	return nil
}

// ListFiltersForCommunity returns the rows the per-community admin page
// renders. Read directly from SQL (not the cache) to surface what is
// actually persisted — the cache is for the hot-path matcher.
func (r *Repo) ListFiltersForCommunity(ctx context.Context, communityID string) ([]Filter, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, community_id, kind, pattern, to_issue, created_by, created_at
		FROM community_mail_filter
		WHERE community_id = ?
		ORDER BY kind, pattern`, communityID)
	if err != nil {
		return nil, err
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
	return out, rows.Err()
}

// InsertIngestIssue links a persisted ingest row to the project_issues
// row that was auto-created from it. The PK is ingest_id so re-running
// the auto-issue path for the same ingest is a no-op (the second
// INSERT trips UNIQUE).
func (r *Repo) InsertIngestIssue(ctx context.Context, ingestID, issueID string) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO email_ingest_issue (ingest_id, issue_id, created_at)
		VALUES (?, ?, ?)`,
		ingestID, issueID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert ingest issue: %w", err)
	}
	return nil
}

// HasIngestIssue tells the auto-issue path whether the issue already
// exists, so retries skip the side-effect.
func (r *Repo) HasIngestIssue(ctx context.Context, ingestID string) (bool, error) {
	var n int
	err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM email_ingest_issue WHERE ingest_id = ?`,
		ingestID).Scan(&n)
	return n > 0, err
}

// FilterByID returns one filter row + whether it exists. Used by the
// CLI reprocess-filter command to validate the input id and pull
// to_issue + community_id without re-querying inside the loop.
func (r *Repo) FilterByID(ctx context.Context, id string) (Filter, error) {
	var f Filter
	var kind string
	var toIssue int
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, kind, pattern, to_issue, created_by, created_at
		FROM community_mail_filter WHERE id = ?`, id).
		Scan(&f.ID, &f.CommunityID, &kind, &f.Pattern, &toIssue, &f.CreatedBy, &created)
	if err != nil {
		return Filter{}, err
	}
	f.Kind = FilterKind(kind)
	f.ToIssue = toIssue != 0
	f.CreatedAt = time.Unix(created, 0).UTC()
	return f, nil
}

// IngestForReprocess is the slim row shape the reprocess-filter CLI
// command iterates. Body text is already persisted (poll worker
// decodeTextBody ran at ingest time); we never re-fetch from IMAP.
type IngestForReprocess struct {
	ID          string
	CommunityID string
	Subject     string
	BodyText    string
}

// IngestsByFilter returns every email_ingest row currently tagged with
// the given matched_filter_id, in ascending received_at so the
// resulting auto-issues sort the same way the originals would have.
// Skips rows that already have an email_ingest_issue link.
func (r *Repo) IngestsByFilter(ctx context.Context, filterID string) ([]IngestForReprocess, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT i.id, COALESCE(i.community_id,''), i.subject, i.body_text
		FROM email_ingest i
		LEFT JOIN email_ingest_issue ij ON ij.ingest_id = i.id
		WHERE i.matched_filter_id = ? AND ij.ingest_id IS NULL
		ORDER BY i.received_at ASC`, filterID)
	if err != nil {
		return nil, fmt.Errorf("ingests by filter: %w", err)
	}
	defer rows.Close()
	out := []IngestForReprocess{}
	for rows.Next() {
		var row IngestForReprocess
		if err := rows.Scan(&row.ID, &row.CommunityID, &row.Subject, &row.BodyText); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AttachmentLookup is the shape AttachmentByID returns — both the row
// and its parent ingest in one structure, since the materialise flow
// needs the folder name (to EXAMINE) + UID + community + project list.
type AttachmentLookup struct {
	Attachment Attachment
	Ingest     Ingest
	FolderName string
}

// AttachmentByID resolves the attachment along with the parent ingest
// + folder name. Returns sql.ErrNoRows when nothing matches.
//
// community_id is nullable since migration 00022 (Unassigned pile),
// so it must scan into sql.NullString here — the original string
// target was crashing every Move on an unassigned email with
// "converting NULL to string is unsupported".
func (r *Repo) AttachmentByID(ctx context.Context, id string) (AttachmentLookup, error) {
	var out AttachmentLookup
	var movedAt sql.NullInt64
	var uploadID, movedProjectID, movedCategory, ingestCommunity sql.NullString
	var receivedAt int64
	var createdEgg int64 // for ingest
	var createdAtt int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT
			a.id, a.ingest_id, a.filename, a.mime, a.size_bytes, a.mime_part_id, a.transfer_encoding,
			a.upload_id, a.moved_to_project_id, a.moved_category, a.moved_at, a.created_at,
			i.id, i.folder_id, i.uid, i.uidvalidity, i.message_id,
			i.from_addr, i.from_name, i.subject, i.received_at,
			i.community_id, i.status, COALESCE(i.matched_filter_id,''), i.created_at,
			f.name
		FROM email_ingest_attachment a
		JOIN email_ingest i        ON i.id = a.ingest_id
		JOIN mailbox_folder f      ON f.id = i.folder_id
		WHERE a.id = ?`, id).Scan(
		&out.Attachment.ID, &out.Attachment.IngestID, &out.Attachment.Filename,
		&out.Attachment.MIME, &out.Attachment.SizeBytes, &out.Attachment.MIMEPartID, &out.Attachment.TransferEncoding,
		&uploadID, &movedProjectID, &movedCategory, &movedAt, &createdAtt,
		&out.Ingest.ID, &out.Ingest.FolderID, &out.Ingest.UID, &out.Ingest.UIDValidity,
		&out.Ingest.MessageID, &out.Ingest.FromAddr, &out.Ingest.FromName, &out.Ingest.Subject,
		&receivedAt, &ingestCommunity, (*string)(&out.Ingest.Status), &out.Ingest.MatchedFilterID,
		&createdEgg, &out.FolderName,
	)
	if err != nil {
		return AttachmentLookup{}, err
	}
	if ingestCommunity.Valid {
		out.Ingest.CommunityID = ingestCommunity.String
	}
	if uploadID.Valid {
		out.Attachment.UploadID = uploadID.String
	}
	if movedProjectID.Valid {
		out.Attachment.MovedToProjectID = movedProjectID.String
	}
	if movedCategory.Valid {
		out.Attachment.MovedCategory = movedCategory.String
	}
	if movedAt.Valid {
		t := time.Unix(movedAt.Int64, 0).UTC()
		out.Attachment.MovedAt = &t
	}
	out.Attachment.CreatedAt = time.Unix(createdAtt, 0).UTC()
	out.Ingest.CreatedAt = time.Unix(createdEgg, 0).UTC()
	out.Ingest.ReceivedAt = time.UnixMilli(receivedAt).UTC()
	return out, nil
}

// AssignIngestCommunity sets community_id for an ingest row that was
// previously NULL. Called when an unassigned attachment is materialised
// into a project — the parent email adopts the project's community.
func (r *Repo) AssignIngestCommunity(ctx context.Context, ingestID, communityID string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE email_ingest SET community_id = ? WHERE id = ? AND community_id IS NULL`,
		communityID, ingestID)
	if err != nil {
		return fmt.Errorf("assign ingest community: %w", err)
	}
	return nil
}

// MarkAttachmentMoved records the materialisation result: the uploads
// row that holds the bytes, the target project, the chosen category.
func (r *Repo) MarkAttachmentMoved(ctx context.Context, attID, uploadID, projectID, category string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE email_ingest_attachment
		SET upload_id = ?, moved_to_project_id = ?, moved_category = ?, moved_at = ?
		WHERE id = ? AND upload_id IS NULL`,
		uploadID, projectID, category, time.Now().Unix(), attID)
	if err != nil {
		return fmt.Errorf("mark attachment moved: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("mailbox: attachment already moved or missing")
	}
	return nil
}

// MarkIngestConsumedIfAllMoved flips the parent email's status to
// 'consumed' once every attachment row has a non-null upload_id. Idempotent.
// Returns whether the status flipped on this call.
func (r *Repo) MarkIngestConsumedIfAllMoved(ctx context.Context, ingestID string) (bool, error) {
	var remaining int
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM email_ingest_attachment
		WHERE ingest_id = ? AND upload_id IS NULL`, ingestID).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining > 0 {
		return false, nil
	}
	res, err := r.DB.ExecContext(ctx, `
		UPDATE email_ingest SET status = 'consumed'
		WHERE id = ? AND status = 'queued'`, ingestID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertAttachments persists attachment metadata for one ingested
// email. Bytes are NOT here — only filename/mime/size/mime_part_id.
// The insert runs inside a single transaction so partial failure is
// recoverable: either every attachment for the message is indexed, or
// none of them are, and the cycle retry on next poll picks it up.
//
// As a side-effect it appends the filenames to the FTS row's
// attachment_names column so "/inbox" search can match on filenames.
func (r *Repo) InsertAttachments(ctx context.Context, ingestID string, parts []ParsedPart) error {
	if len(parts) == 0 {
		return nil
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("attachments tx begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now().Unix()
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		filename := strings.TrimSpace(p.Filename)
		if filename == "" {
			filename = "attachment-" + p.MIMEPartID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO email_ingest_attachment
				(id, ingest_id, filename, mime, size_bytes, mime_part_id, transfer_encoding, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), ingestID, filename, p.MIME, p.SizeBytes, p.MIMEPartID, p.Encoding, now); err != nil {
			return fmt.Errorf("insert attachment %q: %w", filename, err)
		}
		names = append(names, filename)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE email_ingest_fts SET attachment_names = ? WHERE ingest_id = ?`,
		strings.Join(names, " "), ingestID); err != nil {
		return fmt.Errorf("update fts attachment_names: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("attachments tx commit: %w", err)
	}
	return nil
}

// SearchQueueForViewer runs a phrase query against email_ingest_fts and
// returns up to limit matching ingest views scoped to viewer's admin
// community set + optional community pill. Empty queries fall through
// to a normal recent-list (no FTS match). Search is body+attachment
// names + subject + sender — matches "api documentation.doc" both via
// the filename column and the body keyword.
func (r *Repo) SearchQueueForViewer(ctx context.Context, q QueueQuery, query string) ([]QueuedEmailView, error) {
	if strings.TrimSpace(query) == "" {
		views, _, err := r.QueueForViewer(ctx, q)
		return views, err
	}
	if len(q.AdminCommunityIDs) == 0 {
		return []QueuedEmailView{}, nil
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}

	args := []any{query}
	where := []string{"i.status = 'queued'"}
	switch {
	case q.CommunityFilter == UnassignedCommunityID:
		where = append(where, "i.community_id IS NULL")
	case q.CommunityFilter != "":
		where = append(where, "i.community_id = ?")
		args = append(args, q.CommunityFilter)
	default:
		placeholders := strings.Repeat("?,", len(q.AdminCommunityIDs))
		placeholders = placeholders[:len(placeholders)-1]
		where = append(where, "(i.community_id IS NULL OR i.community_id IN ("+placeholders+"))")
		for _, cid := range q.AdminCommunityIDs {
			args = append(args, cid)
		}
	}
	if q.HasAttachments {
		where = append(where, "EXISTS (SELECT 1 FROM email_ingest_attachment a WHERE a.ingest_id = i.id)")
	}
	args = append(args, q.Limit)
	sqlStr := fmt.Sprintf(`
		SELECT i.id, i.community_id, i.from_addr, i.from_name, i.subject,
		       i.body_text, i.received_at, i.status
		FROM email_ingest_fts f
		JOIN email_ingest i ON i.id = f.ingest_id
		WHERE email_ingest_fts MATCH ?
		  AND %s
		ORDER BY i.received_at DESC, i.id DESC
		LIMIT ?`, strings.Join(where, " AND "))

	rows, err := r.DB.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()
	out := []QueuedEmailView{}
	for rows.Next() {
		var v QueuedEmailView
		var communityID sql.NullString
		var received int64
		var status string
		if err := rows.Scan(&v.ID, &communityID, &v.FromAddr, &v.FromName, &v.Subject,
			&v.BodyText, &received, &status); err != nil {
			return nil, err
		}
		if communityID.Valid {
			v.CommunityID = communityID.String
		}
		v.ReceivedAt = time.UnixMilli(received).UTC()
		v.Status = IngestStatus(status)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		if err := r.attachAttachmentsBulk(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
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
	BodyText    string // persisted text body, fed into the expandable row
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
// to see. Rows scope:
//   - q.CommunityFilter == "":    union of viewer's admin communities + unassigned (NULL community_id)
//   - q.CommunityFilter == "_unassigned": only NULL community_id (the "Unassigned" pill)
//   - q.CommunityFilter == "<id>": one community (caller validates this is in AdminCommunityIDs)
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

	switch {
	case q.CommunityFilter == UnassignedCommunityID:
		where = append(where, "i.community_id IS NULL")
	case q.CommunityFilter != "":
		where = append(where, "i.community_id = ?")
		args = append(args, q.CommunityFilter)
	default:
		placeholders := strings.Repeat("?,", len(q.AdminCommunityIDs))
		placeholders = placeholders[:len(placeholders)-1]
		where = append(where, "(i.community_id IS NULL OR i.community_id IN ("+placeholders+"))")
		for _, cid := range q.AdminCommunityIDs {
			args = append(args, cid)
		}
	}

	if q.HasAttachments {
		where = append(where, "EXISTS (SELECT 1 FROM email_ingest_attachment a WHERE a.ingest_id = i.id)")
	}

	if q.Cursor != nil {
		where = append(where, "(i.received_at < ? OR (i.received_at = ? AND i.id < ?))")
		args = append(args, q.Cursor.ReceivedAtUnixMS, q.Cursor.ReceivedAtUnixMS, q.Cursor.ID)
	}

	args = append(args, q.Limit+1) // fetch one extra to detect "has more"

	query := fmt.Sprintf(`
		SELECT i.id, i.community_id, i.from_addr, i.from_name, i.subject,
		       i.body_text, i.received_at, i.status
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
		var communityID sql.NullString
		var received int64
		var status string
		if err := rows.Scan(&v.ID, &communityID, &v.FromAddr, &v.FromName, &v.Subject,
			&v.BodyText, &received, &status); err != nil {
			return nil, nil, err
		}
		if communityID.Valid {
			v.CommunityID = communityID.String
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
