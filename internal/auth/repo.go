package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// --- users ---

// CountUsers returns the total number of rows in `users`. Used by the
// bootstrap-admin flow to detect a brand-new install.
func (r *Repo) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *Repo) CreateUser(ctx context.Context, u User) error {
	now := time.Now().Unix()
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, normEmail(u.Email), u.PasswordHash, string(u.Status), now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrEmailTaken
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *Repo) UserByEmail(ctx context.Context, email string) (User, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, email, password_hash, status, created_at, updated_at
		FROM users WHERE email = ?`, normEmail(email))
	return scanUser(row)
}

func (r *Repo) UserByID(ctx context.Context, id string) (User, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT id, email, password_hash, status, created_at, updated_at
		FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (r *Repo) ActivateUser(ctx context.Context, id string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		string(StatusActive), time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("activate user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanUser(row *sql.Row) (User, error) {
	var u User
	var status string
	var created, updated int64
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &status, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Status = UserStatus(status)
	u.CreatedAt = time.Unix(created, 0)
	u.UpdatedAt = time.Unix(updated, 0)
	return u, nil
}

// --- verification tokens ---

func (r *Repo) CreateVerificationToken(ctx context.Context, token, userID, purpose string, expiresAt time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO verification_tokens (token, user_id, purpose, expires_at)
		VALUES (?, ?, ?, ?)`, token, userID, purpose, expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("create verification token: %w", err)
	}
	return nil
}

type VerificationToken struct {
	Token     string
	UserID    string
	Purpose   string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

func (r *Repo) ConsumeVerificationToken(ctx context.Context, token, purpose string) (VerificationToken, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return VerificationToken{}, err
	}
	defer tx.Rollback()

	var vt VerificationToken
	var exp int64
	var used sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT token, user_id, purpose, expires_at, used_at
		FROM verification_tokens WHERE token = ? AND purpose = ?`, token, purpose).
		Scan(&vt.Token, &vt.UserID, &vt.Purpose, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationToken{}, ErrTokenInvalid
	}
	if err != nil {
		return VerificationToken{}, err
	}
	vt.ExpiresAt = time.Unix(exp, 0)
	if used.Valid {
		t := time.Unix(used.Int64, 0)
		vt.UsedAt = &t
		return VerificationToken{}, ErrTokenInvalid
	}
	if time.Now().After(vt.ExpiresAt) {
		return VerificationToken{}, ErrTokenInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE verification_tokens SET used_at = ? WHERE token = ?`,
		time.Now().Unix(), token); err != nil {
		return VerificationToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationToken{}, err
	}
	return vt, nil
}

// --- invites ---

type InviteCode struct {
	Code        string
	CommunityID string
	CreatedBy   *string
	UsedBy      *string
	UsedAt      *time.Time
	MaxUses     *int
	UsesCount   int
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// Exhausted reports whether the invite has hit its uses cap.
func (i InviteCode) Exhausted() bool {
	return i.MaxUses != nil && i.UsesCount >= *i.MaxUses
}

// CreateInvite creates a new invite code. maxUses=nil means unlimited reuses
// (Discord-style), otherwise the code will be rejected once uses_count hits
// that ceiling.
func (r *Repo) CreateInvite(ctx context.Context, code, communityID string, createdBy *string, maxUses *int, expiresAt time.Time) error {
	var mu sql.NullInt64
	if maxUses != nil {
		mu = sql.NullInt64{Int64: int64(*maxUses), Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO invite_codes (code, community_id, created_by, max_uses, uses_count, expires_at, created_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)`,
		code, communityID, createdBy, mu, expiresAt.Unix(), time.Now().Unix())
	return err
}

// ConsumeInvite validates and increments uses_count for the invite within a
// transaction. The first consumer also populates used_by / used_at for
// backwards-compat with legacy single-use codes.
func (r *Repo) ConsumeInvite(ctx context.Context, tx *sql.Tx, code, userID string) (InviteCode, error) {
	var ic InviteCode
	var createdBy, usedBy sql.NullString
	var usedAt, maxUses sql.NullInt64
	var exp, created int64
	err := tx.QueryRowContext(ctx, `
		SELECT code, community_id, created_by, used_by, used_at, max_uses, uses_count, expires_at, created_at
		FROM invite_codes WHERE code = ?`, code).
		Scan(&ic.Code, &ic.CommunityID, &createdBy, &usedBy, &usedAt, &maxUses, &ic.UsesCount, &exp, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteCode{}, ErrInviteInvalid
	}
	if err != nil {
		return InviteCode{}, err
	}
	if createdBy.Valid {
		ic.CreatedBy = &createdBy.String
	}
	if maxUses.Valid {
		mu := int(maxUses.Int64)
		ic.MaxUses = &mu
	}
	ic.ExpiresAt = time.Unix(exp, 0)
	ic.CreatedAt = time.Unix(created, 0)
	if time.Now().After(ic.ExpiresAt) {
		return InviteCode{}, ErrInviteInvalid
	}
	if ic.Exhausted() {
		return InviteCode{}, ErrInviteExhausted
	}
	// First use also stamps used_by / used_at; subsequent uses just bump count.
	if !usedBy.Valid {
		_, err = tx.ExecContext(ctx, `
			UPDATE invite_codes SET used_by = ?, used_at = ?, uses_count = uses_count + 1 WHERE code = ?`,
			userID, time.Now().Unix(), code)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE invite_codes SET uses_count = uses_count + 1 WHERE code = ?`, code)
	}
	if err != nil {
		return InviteCode{}, err
	}
	ic.UsesCount++
	return ic, nil
}

// ListInvites returns every invite code for the community, newest first.
func (r *Repo) ListInvites(ctx context.Context, communityID string) ([]InviteCode, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT code, community_id, created_by, used_by, used_at, max_uses, uses_count, expires_at, created_at
		FROM invite_codes WHERE community_id = ?
		ORDER BY created_at DESC`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteCode
	for rows.Next() {
		var ic InviteCode
		var createdBy, usedBy sql.NullString
		var usedAt, maxUses sql.NullInt64
		var exp, created int64
		if err := rows.Scan(&ic.Code, &ic.CommunityID, &createdBy, &usedBy, &usedAt, &maxUses, &ic.UsesCount, &exp, &created); err != nil {
			return nil, err
		}
		if createdBy.Valid {
			ic.CreatedBy = &createdBy.String
		}
		if usedBy.Valid {
			ic.UsedBy = &usedBy.String
		}
		if usedAt.Valid {
			t := time.Unix(usedAt.Int64, 0)
			ic.UsedAt = &t
		}
		if maxUses.Valid {
			mu := int(maxUses.Int64)
			ic.MaxUses = &mu
		}
		ic.ExpiresAt = time.Unix(exp, 0)
		ic.CreatedAt = time.Unix(created, 0)
		out = append(out, ic)
	}
	return out, rows.Err()
}

func (r *Repo) RevokeInvite(ctx context.Context, code string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM invite_codes WHERE code = ?`, code)
	return err
}

// --- memberships ---

func (r *Repo) CreateMembership(ctx context.Context, tx *sql.Tx, m Membership) error {
	exec := r.DB.ExecContext
	if tx != nil {
		exec = tx.ExecContext
	}
	var banned, approved sql.NullInt64
	if m.BannedUntil != nil {
		banned = sql.NullInt64{Int64: m.BannedUntil.Unix(), Valid: true}
	}
	if m.ApprovedAt != nil {
		approved = sql.NullInt64{Int64: m.ApprovedAt.Unix(), Valid: true}
	}
	_, err := exec(ctx, `
		INSERT INTO memberships (id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, approved_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.UserID, m.CommunityID, m.DisplayName, m.AvatarURL, string(m.Role), m.TrustLevel, banned, approved, time.Now().Unix())
	return err
}

func (r *Repo) MembershipFor(ctx context.Context, userID, communityID string) (Membership, error) {
	var m Membership
	var role string
	var banned, approved sql.NullInt64
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, approved_at, created_at
		FROM memberships WHERE user_id = ? AND community_id = ?`, userID, communityID).
		Scan(&m.ID, &m.UserID, &m.CommunityID, &m.DisplayName, &m.AvatarURL, &role, &m.TrustLevel, &banned, &approved, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	m.Role = Role(role)
	if banned.Valid {
		t := time.Unix(banned.Int64, 0)
		m.BannedUntil = &t
	}
	if approved.Valid {
		t := time.Unix(approved.Int64, 0)
		m.ApprovedAt = &t
	}
	m.CreatedAt = time.Unix(created, 0)
	return m, nil
}

func (r *Repo) UpdateMembershipProfile(ctx context.Context, membershipID, displayName, avatarURL string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE memberships SET display_name = ?, avatar_url = ? WHERE id = ?`,
		displayName, avatarURL, membershipID)
	return err
}

// UserIDsByDisplayName resolves a list of (case-insensitive) display
// names to the user_ids backing the matching memberships in this
// community. Used by chat to map @mention tokens to push targets.
// Empty input returns an empty slice without hitting the DB.
func (r *Repo) UserIDsByDisplayName(ctx context.Context, communityID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	placeholders := make([]string, 0, len(names))
	args := []any{communityID}
	for _, n := range names {
		placeholders = append(placeholders, "?")
		args = append(args, n)
	}
	q := `
		SELECT DISTINCT user_id
		FROM memberships
		WHERE community_id = ?
		AND lower(display_name) IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// MemberHit is one row returned by SearchMembersByDisplayName, carrying
// just the columns the @mention popup needs (user id for deterministic
// telegram-style colouring, display name for the row label).
type MemberHit struct {
	UserID      string
	DisplayName string
}

// SearchMembersByDisplayName returns up to `limit` memberships in the
// given community whose display_name starts with the (case-insensitive)
// prefix. Empty prefix returns no rows — the caller is the @mention
// typeahead and wants nothing on an empty token. Ordered by name for
// stable UI.
func (r *Repo) SearchMembersByDisplayName(ctx context.Context, communityID, prefix string, limit int) ([]MemberHit, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 7
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT user_id, display_name
		FROM memberships
		WHERE community_id = ?
		AND lower(display_name) LIKE ? || '%'
		ORDER BY display_name
		LIMIT ?`,
		communityID, strings.ToLower(prefix), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberHit
	for rows.Next() {
		var h MemberHit
		if err := rows.Scan(&h.UserID, &h.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UpdateAllMembershipProfiles updates display_name + avatar_url on every
// membership this user holds, so the profile editor reflects across every
// community at once. Without this, only the membership the user is
// currently viewing got the new name and other communities kept the
// initial email-localpart fallback that admin.PostAddMember assigns.
func (r *Repo) UpdateAllMembershipProfiles(ctx context.Context, userID, displayName, avatarURL string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE memberships SET display_name = ?, avatar_url = ? WHERE user_id = ?`,
		displayName, avatarURL, userID)
	return err
}

func (r *Repo) UpdateMembershipRole(ctx context.Context, membershipID string, role Role) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE memberships SET role = ? WHERE id = ?`, string(role), membershipID)
	return err
}

func (r *Repo) UpdateBan(ctx context.Context, membershipID string, until *time.Time) error {
	var v sql.NullInt64
	if until != nil {
		v = sql.NullInt64{Int64: until.Unix(), Valid: true}
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE memberships SET banned_until = ? WHERE id = ?`, v, membershipID)
	return err
}

// ApproveMembership stamps approved_at = NOW, letting the user past the
// /pending gate.
func (r *Repo) ApproveMembership(ctx context.Context, membershipID string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE memberships SET approved_at = ? WHERE id = ? AND approved_at IS NULL`,
		time.Now().Unix(), membershipID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RejectMembership deletes the membership row outright. The user account
// stays around but they're no longer a member of the community. They can
// re-register with a fresh invite if they want.
func (r *Repo) RejectMembership(ctx context.Context, membershipID string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM memberships WHERE id = ?`, membershipID)
	return err
}

// MemberRow joins memberships with users so the admin UI can show emails.
type MemberRow struct {
	Membership
	Email string
}

// ListPendingMemberships returns memberships with approved_at IS NULL —
// these are the join requests awaiting admin review.
func (r *Repo) ListPendingMemberships(ctx context.Context, communityID string) ([]MemberRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.user_id, m.community_id, m.display_name, m.avatar_url, m.role,
		       m.trust_level, m.banned_until, m.approved_at, m.created_at, u.email
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.community_id = ? AND m.approved_at IS NULL
		ORDER BY m.created_at ASC`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemberRows(rows)
}

// ListMembers returns approved memberships for the admin UI.
func (r *Repo) ListMembers(ctx context.Context, communityID string) ([]MemberRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, m.user_id, m.community_id, m.display_name, m.avatar_url, m.role,
		       m.trust_level, m.banned_until, m.approved_at, m.created_at, u.email
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.community_id = ? AND m.approved_at IS NOT NULL
		ORDER BY m.display_name ASC`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemberRows(rows)
}

func scanMemberRows(rows *sql.Rows) ([]MemberRow, error) {
	var out []MemberRow
	for rows.Next() {
		var m Membership
		var role string
		var banned, approved sql.NullInt64
		var created int64
		var email string
		if err := rows.Scan(&m.ID, &m.UserID, &m.CommunityID, &m.DisplayName, &m.AvatarURL,
			&role, &m.TrustLevel, &banned, &approved, &created, &email); err != nil {
			return nil, err
		}
		m.Role = Role(role)
		if banned.Valid {
			t := time.Unix(banned.Int64, 0)
			m.BannedUntil = &t
		}
		if approved.Valid {
			t := time.Unix(approved.Int64, 0)
			m.ApprovedAt = &t
		}
		m.CreatedAt = time.Unix(created, 0)
		out = append(out, MemberRow{Membership: m, Email: email})
	}
	return out, rows.Err()
}

// CleanupOptions toggles which content a ban should wipe.
type CleanupOptions struct {
	Chat    bool
	Threads bool
	Posts   bool
}

// CleanupUserContent soft-deletes the user's content per the supplied
// options. Soft-delete preserves audit trail (mod-visible content stays).
func (r *Repo) CleanupUserContent(ctx context.Context, userID, communityID string, opts CleanupOptions) error {
	now := time.Now().Unix()
	if opts.Chat {
		if _, err := r.DB.ExecContext(ctx, `
			UPDATE chat_messages SET deleted_at = ?
			WHERE author_id = ? AND community_id = ? AND deleted_at IS NULL`,
			now, userID, communityID); err != nil {
			return fmt.Errorf("cleanup chat: %w", err)
		}
	}
	if opts.Threads {
		if _, err := r.DB.ExecContext(ctx, `
			UPDATE threads SET deleted_at = ?
			WHERE author_id = ? AND community_id = ? AND deleted_at IS NULL`,
			now, userID, communityID); err != nil {
			return fmt.Errorf("cleanup threads: %w", err)
		}
	}
	if opts.Posts {
		if _, err := r.DB.ExecContext(ctx, `
			UPDATE posts SET deleted_at = ?
			WHERE author_id = ? AND deleted_at IS NULL
			AND thread_id IN (SELECT id FROM threads WHERE community_id = ?)`,
			now, userID, communityID); err != nil {
			return fmt.Errorf("cleanup posts: %w", err)
		}
	}
	return nil
}

// AdminCommunityIDs returns the community IDs in which the user holds
// admin OR moderator role AND is approved. Returns an empty slice (not
// nil) when there are none. Drives the global /inbox gate plus the
// per-row community scoping inside the mailbox feature.
func (r *Repo) AdminCommunityIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT community_id FROM memberships
		WHERE user_id = ?
		  AND role IN (?, ?)
		  AND approved_at IS NOT NULL`,
		userID, string(RoleAdmin), string(RoleMod))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out = append(out, cid)
	}
	return out, rows.Err()
}

// CountAdmins returns the number of admin-role memberships in the
// community. Used as a last-admin-standing guard before remove/demote
// so an op can't accidentally lock everyone out.
func (r *Repo) CountAdmins(ctx context.Context, communityID string) (int, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships
		WHERE community_id = ? AND role = ?`,
		communityID, string(RoleAdmin)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MembershipByID is needed by admin operations that come in via signal/ID.
func (r *Repo) MembershipByID(ctx context.Context, id string) (Membership, error) {
	var m Membership
	var role string
	var banned, approved sql.NullInt64
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, approved_at, created_at
		FROM memberships WHERE id = ?`, id).
		Scan(&m.ID, &m.UserID, &m.CommunityID, &m.DisplayName, &m.AvatarURL, &role, &m.TrustLevel, &banned, &approved, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	m.Role = Role(role)
	if banned.Valid {
		t := time.Unix(banned.Int64, 0)
		m.BannedUntil = &t
	}
	if approved.Valid {
		t := time.Unix(approved.Int64, 0)
		m.ApprovedAt = &t
	}
	m.CreatedAt = time.Unix(created, 0)
	return m, nil
}
