package rooms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("rooms: not found")

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// EnsureSeeded makes sure the community has all NumRooms slots present.
// Idempotent: any missing slot gets inserted with a deterministic id of
// the form "<communityID>:room-NN". Call from main.go on boot for the
// bootstrap community, and lazily from GetGrid for any others.
func (r *Repo) EnsureSeeded(ctx context.Context, communityID string) error {
	now := time.Now().UTC().UnixMilli()
	for slot := 1; slot <= NumRooms; slot++ {
		id := fmt.Sprintf("%s:room-%02d", communityID, slot)
		name := fmt.Sprintf("Room %d", slot)
		_, err := r.DB.ExecContext(ctx, `
			INSERT OR IGNORE INTO rooms
			  (id, community_id, slot, name, is_public, admin_user_id, created_at, updated_at)
			VALUES (?,?,?,?,0,NULL,?,?)`,
			id, communityID, slot, name, now, now)
		if err != nil {
			return fmt.Errorf("seed room slot %d: %w", slot, err)
		}
	}
	return nil
}

// ListRoomsForCommunity returns the 8 rooms of one community, ordered by slot.
func (r *Repo) ListRoomsForCommunity(ctx context.Context, communityID string) ([]Room, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, community_id, slot, name, is_public,
		       COALESCE(admin_user_id,''), created_at, updated_at
		FROM rooms WHERE community_id = ? ORDER BY slot ASC`,
		communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Room, 0, NumRooms)
	for rows.Next() {
		var rm Room
		var pub int
		var cAt, uAt int64
		if err := rows.Scan(&rm.ID, &rm.CommunityID, &rm.Slot, &rm.Name, &pub,
			&rm.AdminUserID, &cAt, &uAt); err != nil {
			return nil, err
		}
		rm.IsPublic = pub != 0
		rm.CreatedAt = time.UnixMilli(cAt).UTC()
		rm.UpdatedAt = time.UnixMilli(uAt).UTC()
		out = append(out, rm)
	}
	return out, rows.Err()
}

func (r *Repo) RoomByID(ctx context.Context, id string) (Room, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, community_id, slot, name, is_public,
		       COALESCE(admin_user_id,''), created_at, updated_at
		FROM rooms WHERE id = ?`, id)
	var rm Room
	var pub int
	var cAt, uAt int64
	err := row.Scan(&rm.ID, &rm.CommunityID, &rm.Slot, &rm.Name, &pub,
		&rm.AdminUserID, &cAt, &uAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, ErrNotFound
	}
	if err != nil {
		return Room{}, err
	}
	rm.IsPublic = pub != 0
	rm.CreatedAt = time.UnixMilli(cAt).UTC()
	rm.UpdatedAt = time.UnixMilli(uAt).UTC()
	return rm, nil
}

func (r *Repo) UpdateRoomName(ctx context.Context, id, name string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE rooms SET name = ?, updated_at = ? WHERE id = ?`,
		name, now.UnixMilli(), id)
	return err
}

func (r *Repo) SetAdmin(ctx context.Context, id, userID string, now time.Time) error {
	var u any
	if userID != "" {
		u = userID
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE rooms SET admin_user_id = ?, updated_at = ? WHERE id = ?`,
		u, now.UnixMilli(), id)
	return err
}

func (r *Repo) SetPublic(ctx context.Context, id string, pub bool, now time.Time) error {
	v := 0
	if pub {
		v = 1
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE rooms SET is_public = ?, updated_at = ? WHERE id = ?`,
		v, now.UnixMilli(), id)
	return err
}

// BumpUpdatedAt bumps updated_at, used after live-state changes to nudge
// any per-room metadata caches.
func (r *Repo) BumpUpdatedAt(ctx context.Context, id string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE rooms SET updated_at = ? WHERE id = ?`, now.UnixMilli(), id)
	return err
}

func (r *Repo) AppendChat(ctx context.Context, m ChatMessage) error {
	var uid any
	if m.AuthorUserID != "" {
		uid = m.AuthorUserID
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO room_chat
		  (id, room_id, community_id, author_user_id, author_name, body, body_html, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, m.RoomID, m.CommunityID, uid, m.AuthorName, m.Body, m.BodyHTML, m.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert room_chat: %w", err)
	}
	return nil
}

// ListChat returns up to `limit` most-recent messages, oldest-first.
func (r *Repo) ListChat(ctx context.Context, roomID string, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, room_id, community_id, COALESCE(author_user_id,''), author_name, body, body_html, created_at
		FROM room_chat
		WHERE room_id = ?
		ORDER BY created_at DESC LIMIT ?`, roomID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var ts int64
		if err := rows.Scan(&m.ID, &m.RoomID, &m.CommunityID, &m.AuthorUserID, &m.AuthorName,
			&m.Body, &m.BodyHTML, &ts); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(ts).UTC()
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func (r *Repo) CreateInvite(ctx context.Context, inv Invite) error {
	var exp, rev any
	if inv.ExpiresAt != nil {
		exp = inv.ExpiresAt.UnixMilli()
	}
	if inv.RevokedAt != nil {
		rev = inv.RevokedAt.UnixMilli()
	}
	var by any
	if inv.CreatedBy != "" {
		by = inv.CreatedBy
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO room_invites (token, room_id, created_by, created_at, expires_at, revoked_at)
		VALUES (?,?,?,?,?,?)`,
		inv.Token, inv.RoomID, by, inv.CreatedAt.UnixMilli(), exp, rev)
	if err != nil {
		return fmt.Errorf("insert room_invites: %w", err)
	}
	return nil
}

func (r *Repo) InviteByToken(ctx context.Context, token string) (Invite, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT token, room_id, COALESCE(created_by,''), created_at, expires_at, revoked_at
		FROM room_invites WHERE token = ?`, token)
	var inv Invite
	var cAt int64
	var exp, rev sql.NullInt64
	err := row.Scan(&inv.Token, &inv.RoomID, &inv.CreatedBy, &cAt, &exp, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	inv.CreatedAt = time.UnixMilli(cAt).UTC()
	if exp.Valid {
		t := time.UnixMilli(exp.Int64).UTC()
		inv.ExpiresAt = &t
	}
	if rev.Valid {
		t := time.UnixMilli(rev.Int64).UTC()
		inv.RevokedAt = &t
	}
	return inv, nil
}

// ActiveInviteForRoom returns the most recent non-revoked, non-expired invite,
// or ErrNotFound. Only one "active" invite is exposed at a time in the UI; the
// admin rotates by creating a new one + revoking the prior.
func (r *Repo) ActiveInviteForRoom(ctx context.Context, roomID string) (Invite, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT token, room_id, COALESCE(created_by,''), created_at, expires_at, revoked_at
		FROM room_invites
		WHERE room_id = ? AND revoked_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, roomID)
	if err != nil {
		return Invite{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Invite{}, ErrNotFound
	}
	var inv Invite
	var cAt int64
	var exp, rev sql.NullInt64
	if err := rows.Scan(&inv.Token, &inv.RoomID, &inv.CreatedBy, &cAt, &exp, &rev); err != nil {
		return Invite{}, err
	}
	inv.CreatedAt = time.UnixMilli(cAt).UTC()
	if exp.Valid {
		t := time.UnixMilli(exp.Int64).UTC()
		inv.ExpiresAt = &t
	}
	if rev.Valid {
		t := time.UnixMilli(rev.Int64).UTC()
		inv.RevokedAt = &t
	}
	now := time.Now().UTC()
	if !inv.Active(now) {
		return Invite{}, ErrNotFound
	}
	return inv, nil
}

// displayNameForUser mirrors privatemsg.Repo.DisplayName — picks the most
// recent membership display_name. Returns empty when the user has no rows.
func (r *Repo) displayNameForUser(ctx context.Context, userID string) (string, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT effective_display_name FROM memberships
		WHERE user_id = ? ORDER BY created_at DESC LIMIT 1`, userID)
	var n string
	err := row.Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return n, nil
}

func (r *Repo) RevokeInvite(ctx context.Context, token string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE room_invites SET revoked_at = ? WHERE token = ? AND revoked_at IS NULL`,
		now.UnixMilli(), token)
	return err
}

// RevokeAllInvites revokes every still-active invite for a room in one
// statement. Used by the empty-room reset so stale share-links can't be
// reused for the next session.
func (r *Repo) RevokeAllInvites(ctx context.Context, roomID string, now time.Time) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE room_invites SET revoked_at = ? WHERE room_id = ? AND revoked_at IS NULL`,
		now.UnixMilli(), roomID)
	return err
}

// ArchiveChat moves a room's live chat into room_chat_archive, then clears
// the live rows — leaving the next session a blank chat while the prior
// conversation is retained. Runs as one transaction so a viewer can never
// observe a half-archived chat.
func (r *Repo) ArchiveChat(ctx context.Context, roomID string, now time.Time) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO room_chat_archive
		  (id, room_id, community_id, author_user_id, author_name, body, body_html, created_at, archived_at)
		SELECT id, room_id, community_id, author_user_id, author_name, body, body_html, created_at, ?
		FROM room_chat WHERE room_id = ?`,
		now.UnixMilli(), roomID); err != nil {
		return fmt.Errorf("archive room_chat: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM room_chat WHERE room_id = ?`, roomID); err != nil {
		return fmt.Errorf("clear room_chat: %w", err)
	}
	return tx.Commit()
}
