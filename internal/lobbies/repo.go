// Package lobbies implements tokenized guest access to a community.
// One lobby = one host (community admin/mod) talking to one guest who
// joined via a shared URL — no account required. Messages persist;
// images upload through the existing uploads.Store using a synthetic
// `lobby:<id>` user id so the standard signed-URL pipeline works.
package lobbies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status values stored in the `status` column. Open = guest URL serves
// the chat; archived = hidden from host's default list but URL still
// works; closed = guest URL returns 410, host retains history.
const (
	StatusOpen     = "open"
	StatusArchived = "archived"
	StatusClosed   = "closed"
)

// Medium values stored in the `medium` column. v1 only mints lobby; the
// room variant is reserved for the paired-video extension (Phase B).
const (
	MediumLobby = "lobby"
	MediumRoom  = "room"
)

// AuthorKind values stored in lobby_messages.author_kind.
const (
	AuthorHost  = "host"
	AuthorGuest = "guest"
)

// ErrNotFound is returned when a lookup misses. ErrTokenTaken surfaces
// the UNIQUE-violation on guest_token so the service layer can retry
// with a fresh token instead of bubbling a raw sqlite error.
var (
	ErrNotFound   = errors.New("lobby not found")
	ErrTokenTaken = errors.New("guest token already in use")
)

// Lobby mirrors a row in the `lobbies` table. ExpiresAt is nil when no
// expiry was set.
type Lobby struct {
	ID               string
	CommunityID      string
	HostUserID       string
	Medium           string
	GuestDisplayName string
	GuestEmail       string
	GuestToken       string
	Status           string
	ExpiresAt        *time.Time
	CreatedAt        time.Time
	LastActivityAt   time.Time
}

// IsExpired reports whether ExpiresAt is set and has passed.
func (l Lobby) IsExpired(now time.Time) bool {
	return l.ExpiresAt != nil && now.After(*l.ExpiresAt)
}

// IsOpen reports whether the guest URL should serve the chat. Closed
// lobbies always refuse; expired open lobbies are treated as closed.
func (l Lobby) IsOpen(now time.Time) bool {
	if l.Status != StatusOpen {
		return false
	}
	return !l.IsExpired(now)
}

// LobbyMessage mirrors a row in `lobby_messages`. AuthorUserID is nil
// for guest messages (the guest has no user account).
type LobbyMessage struct {
	ID           string
	LobbyID      string
	AuthorKind   string
	AuthorUserID *string
	BodyMarkdown string
	BodyHTML     string
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// IsDeleted is a convenience for templ render branches.
func (m LobbyMessage) IsDeleted() bool { return m.DeletedAt != nil }

// LobbyRow extends Lobby with denormalised columns needed by the host's
// list view (last message preview + message count). Cheap because the
// host list page paginates aggressively.
type LobbyRow struct {
	Lobby
	MessageCount   int
	LastMessageAt  *time.Time
	LastAuthorKind string
}

// Repo is the SQL gateway. Stateless — all state lives in the DB.
type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// Create inserts a Lobby row. The caller mints the ID, token, and
// timestamps; this method just persists.
func (r *Repo) Create(ctx context.Context, l Lobby) error {
	var exp sql.NullInt64
	if l.ExpiresAt != nil {
		exp = sql.NullInt64{Int64: l.ExpiresAt.Unix(), Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO lobbies (id, community_id, host_user_id, medium,
			guest_display_name, guest_email, guest_token, status,
			expires_at, created_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.CommunityID, l.HostUserID, l.Medium,
		l.GuestDisplayName, l.GuestEmail, l.GuestToken, l.Status,
		exp, l.CreatedAt.Unix(), l.LastActivityAt.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrTokenTaken
		}
		return fmt.Errorf("create lobby: %w", err)
	}
	return nil
}

// ByID fetches by primary key. Used by host-side handlers that read the
// id from the URL.
func (r *Repo) ByID(ctx context.Context, id string) (Lobby, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, host_user_id, medium,
			guest_display_name, guest_email, guest_token, status,
			expires_at, created_at, last_activity_at
		FROM lobbies WHERE id = ?`, id)
	return scanLobby(row.Scan)
}

// ByToken fetches by the shared guest_token. The unique index makes
// this O(log n).
func (r *Repo) ByToken(ctx context.Context, token string) (Lobby, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, host_user_id, medium,
			guest_display_name, guest_email, guest_token, status,
			expires_at, created_at, last_activity_at
		FROM lobbies WHERE guest_token = ?`, token)
	return scanLobby(row.Scan)
}

// ListByCommunity returns lobbies in the community filtered by status,
// ordered newest activity first. Pass empty string to return every
// status.
func (r *Repo) ListByCommunity(ctx context.Context, communityID, status string) ([]LobbyRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	const baseSQL = `
		SELECT l.id, l.community_id, l.host_user_id, l.medium,
			l.guest_display_name, l.guest_email, l.guest_token, l.status,
			l.expires_at, l.created_at, l.last_activity_at,
			COALESCE(m.cnt, 0) AS message_count,
			m.last_at, m.last_kind
		FROM lobbies l
		LEFT JOIN (
			SELECT lobby_id,
				COUNT(*)              AS cnt,
				MAX(created_at)       AS last_at,
				(SELECT author_kind FROM lobby_messages
				 WHERE lobby_id = lm.lobby_id
				 ORDER BY created_at DESC LIMIT 1) AS last_kind
			FROM lobby_messages lm
			WHERE deleted_at IS NULL
			GROUP BY lobby_id
		) m ON m.lobby_id = l.id
		WHERE l.community_id = ?`
	if status == "" {
		rows, err = r.DB.QueryContext(ctx,
			baseSQL+` ORDER BY l.last_activity_at DESC`, communityID)
	} else {
		rows, err = r.DB.QueryContext(ctx,
			baseSQL+` AND l.status = ? ORDER BY l.last_activity_at DESC`,
			communityID, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LobbyRow
	for rows.Next() {
		var (
			r   LobbyRow
			exp sql.NullInt64
			created, lastAct int64
			cnt    int
			lastAt sql.NullInt64
			lastKind sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.CommunityID, &r.HostUserID, &r.Medium,
			&r.GuestDisplayName, &r.GuestEmail, &r.GuestToken, &r.Status,
			&exp, &created, &lastAct,
			&cnt, &lastAt, &lastKind); err != nil {
			return nil, err
		}
		if exp.Valid {
			t := time.Unix(exp.Int64, 0)
			r.ExpiresAt = &t
		}
		r.CreatedAt = time.Unix(created, 0)
		r.LastActivityAt = time.Unix(lastAct, 0)
		r.MessageCount = cnt
		if lastAt.Valid {
			t := time.Unix(lastAt.Int64, 0)
			r.LastMessageAt = &t
		}
		if lastKind.Valid {
			r.LastAuthorKind = lastKind.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateStatus toggles the lobby state machine. Caller validates the
// transition.
func (r *Repo) UpdateStatus(ctx context.Context, id, status string) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE lobbies SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateGuestProfile stamps name + email. Called by Join when the guest
// first arrives, and by host-side edits.
func (r *Repo) UpdateGuestProfile(ctx context.Context, id, name, email string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE lobbies SET guest_display_name = ?, guest_email = ?
		WHERE id = ?`, name, email, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchActivity bumps last_activity_at. Called after every message.
func (r *Repo) TouchActivity(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE lobbies SET last_activity_at = ? WHERE id = ?`,
		time.Now().Unix(), id)
	return err
}

// Delete hard-deletes the lobby. ON DELETE CASCADE on lobby_messages
// drops the history along with it. Use only for admin-driven cleanup.
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM lobbies WHERE id = ?`, id)
	return err
}

// AppendMessage persists a single message. Caller mints the ID +
// renders BodyHTML.
func (r *Repo) AppendMessage(ctx context.Context, m LobbyMessage) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO lobby_messages (id, lobby_id, author_kind,
			author_user_id, body_md, body_html, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.LobbyID, m.AuthorKind, m.AuthorUserID,
		m.BodyMarkdown, m.BodyHTML, m.CreatedAt.Unix())
	return err
}

// RecentMessages returns the latest `limit` messages, newest first. The
// host and guest views render in reverse (oldest at top); the templ
// handles the flip the same way ChatPage does.
func (r *Repo) RecentMessages(ctx context.Context, lobbyID string, limit int) ([]LobbyMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, lobby_id, author_kind, author_user_id,
			body_md, body_html, created_at, deleted_at
		FROM lobby_messages
		WHERE lobby_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, lobbyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LobbyMessage
	for rows.Next() {
		var (
			m       LobbyMessage
			aid     sql.NullString
			created int64
			del     sql.NullInt64
		)
		if err := rows.Scan(&m.ID, &m.LobbyID, &m.AuthorKind, &aid,
			&m.BodyMarkdown, &m.BodyHTML, &created, &del); err != nil {
			return nil, err
		}
		if aid.Valid {
			s := aid.String
			m.AuthorUserID = &s
		}
		m.CreatedAt = time.Unix(created, 0)
		if del.Valid {
			t := time.Unix(del.Int64, 0)
			m.DeletedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanLobby(scan func(dest ...any) error) (Lobby, error) {
	var (
		l       Lobby
		exp     sql.NullInt64
		created int64
		lastAct int64
	)
	err := scan(&l.ID, &l.CommunityID, &l.HostUserID, &l.Medium,
		&l.GuestDisplayName, &l.GuestEmail, &l.GuestToken, &l.Status,
		&exp, &created, &lastAct)
	if errors.Is(err, sql.ErrNoRows) {
		return Lobby{}, ErrNotFound
	}
	if err != nil {
		return Lobby{}, err
	}
	if exp.Valid {
		t := time.Unix(exp.Int64, 0)
		l.ExpiresAt = &t
	}
	l.CreatedAt = time.Unix(created, 0)
	l.LastActivityAt = time.Unix(lastAct, 0)
	return l, nil
}
