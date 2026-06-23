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

// --- oauth identities ---

// OAuthIdentity links an external provider account to a local user. The cached
// email/name/avatar are refreshed on every sign-in.
type OAuthIdentity struct {
	Provider       string
	ProviderUserID string
	UserID         string
	Email          string
	Name           string
	AvatarURL      string
}

// UserIDByIdentity returns the local user id linked to a provider account, or
// ErrNotFound when no link exists yet.
func (r *Repo) UserIDByIdentity(ctx context.Context, provider, providerUserID string) (string, error) {
	var uid string
	err := r.DB.QueryRowContext(ctx, `
		SELECT user_id FROM user_identities
		WHERE provider = ? AND provider_user_id = ?`, provider, providerUserID).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return uid, nil
}

// LinkIdentity records (or refreshes) a provider↔user link. Idempotent: a
// repeated link for the same (provider, provider_user_id) updates the cached
// email/name/avatar. Pass tx to enlist in a transaction, or nil to run alone.
func (r *Repo) LinkIdentity(ctx context.Context, tx *sql.Tx, id OAuthIdentity) error {
	const q = `
		INSERT INTO user_identities (provider, provider_user_id, user_id, email, name, avatar_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, provider_user_id) DO UPDATE SET
			email = excluded.email, name = excluded.name, avatar_url = excluded.avatar_url`
	args := []any{id.Provider, id.ProviderUserID, id.UserID, normEmail(id.Email), id.Name, id.AvatarURL, time.Now().Unix()}
	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, q, args...)
	} else {
		_, err = r.DB.ExecContext(ctx, q, args...)
	}
	if err != nil {
		return fmt.Errorf("link identity: %w", err)
	}
	return nil
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

// PeekVerificationToken validates a token (right purpose, unused, unexpired)
// WITHOUT consuming it. Used to render a confirmation page before the user
// commits — the matching POST then calls ConsumeVerificationToken to burn it.
func (r *Repo) PeekVerificationToken(ctx context.Context, token, purpose string) (VerificationToken, error) {
	var vt VerificationToken
	var exp int64
	var used sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `
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
	if used.Valid || time.Now().After(vt.ExpiresAt) {
		return VerificationToken{}, ErrTokenInvalid
	}
	return vt, nil
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
		INSERT INTO memberships (id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, approved_at, created_at, join_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.UserID, m.CommunityID, m.DisplayName, m.AvatarURL, string(m.Role), m.TrustLevel, banned, approved, time.Now().Unix(), m.JoinReason)
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
		       m.trust_level, m.banned_until, m.approved_at, m.created_at, m.join_reason, u.email
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
		       m.trust_level, m.banned_until, m.approved_at, m.created_at, m.join_reason, u.email
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
			&role, &m.TrustLevel, &banned, &approved, &created, &m.JoinReason, &email); err != nil {
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

// OldestCommunityAdminID returns the user_id of the longest-tenured
// privileged member (admin OR owner) in the given community. Used as the
// system-fallback creator when MAILBOX_SYSTEM_USER_ID is unset. Owner is
// included because migration 00055 promotes the sole admin to owner, so an
// owner-only community would otherwise return ErrNoRows.
func (r *Repo) OldestCommunityAdminID(ctx context.Context, communityID string) (string, error) {
	var userID string
	err := r.DB.QueryRowContext(ctx, `
		SELECT user_id FROM memberships
		WHERE community_id = ? AND role IN (?, ?) AND approved_at IS NOT NULL
		ORDER BY created_at ASC
		LIMIT 1`,
		communityID, string(RoleAdmin), string(RoleOwner)).Scan(&userID)
	return userID, err
}

// AdminCommunityIDs returns the community IDs in which the user holds a
// privileged role (owner, admin OR moderator) AND is approved. Returns an
// empty slice (not nil) when there are none. Drives the global /inbox gate
// plus the per-row community scoping inside the mailbox feature. Owner is
// included so a promoted owner (migration 00055) keeps admin-scoped surfaces.
func (r *Repo) AdminCommunityIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT community_id FROM memberships
		WHERE user_id = ?
		  AND role IN (?, ?, ?)
		  AND approved_at IS NOT NULL`,
		userID, string(RoleAdmin), string(RoleMod), string(RoleOwner))
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

// GlobalUser is one row of the /superadmin user roster: a user plus how
// many communities they belong to.
type GlobalUser struct {
	ID             string
	Email          string
	Status         UserStatus
	CreatedAt      time.Time
	CommunityCount int
}

// ListAllUsers returns every user with their membership count, newest
// first. Drives the platform super-admin user roster.
func (r *Repo) ListAllUsers(ctx context.Context) ([]GlobalUser, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT u.id, u.email, u.status, u.created_at,
		       (SELECT COUNT(*) FROM memberships mb WHERE mb.user_id = u.id) AS community_count
		FROM users u
		ORDER BY u.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GlobalUser
	for rows.Next() {
		var g GlobalUser
		var status string
		var created int64
		if err := rows.Scan(&g.ID, &g.Email, &status, &created, &g.CommunityCount); err != nil {
			return nil, err
		}
		g.Status = UserStatus(status)
		g.CreatedAt = time.Unix(created, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// UserMembership is one community a user belongs to, enriched with the
// membership id (so platform actions can target it directly), role,
// approval/ban state and per-community activity counts. Drives the
// /superadmin user drill-down — "which communities is this user in, and
// what have they been doing in each".
type UserMembership struct {
	MembershipID string
	CommunityID  string
	Slug         string
	Name         string
	Role         Role
	IsApproved   bool
	BannedUntil  *time.Time
	ChatCount    int
	ThreadCount  int
	LastActive   *time.Time // most recent chat message in this community
}

// UserMemberships returns every community the user belongs to with their
// membership id, role, state and live activity counts, ordered by community
// name. Counts ignore soft-deleted rows so they reflect what's still visible.
func (r *Repo) UserMemberships(ctx context.Context, userID string) ([]UserMembership, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT m.id, c.id, c.slug, c.name, m.role,
		       COALESCE(m.approved_at, 0),
		       COALESCE(m.banned_until, 0),
		       (SELECT COUNT(*) FROM chat_messages cm
		          WHERE cm.author_id = m.user_id AND cm.community_id = c.id AND cm.deleted_at IS NULL),
		       (SELECT COUNT(*) FROM threads t
		          WHERE t.author_id = m.user_id AND t.community_id = c.id AND t.deleted_at IS NULL),
		       (SELECT MAX(cm.created_at) FROM chat_messages cm
		          WHERE cm.author_id = m.user_id AND cm.community_id = c.id AND cm.deleted_at IS NULL)
		FROM memberships m
		JOIN communities c ON c.id = m.community_id
		WHERE m.user_id = ?
		ORDER BY c.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserMembership
	for rows.Next() {
		var um UserMembership
		var role string
		var approvedAt, bannedUntil int64
		var lastActive sql.NullInt64
		if err := rows.Scan(&um.MembershipID, &um.CommunityID, &um.Slug, &um.Name, &role,
			&approvedAt, &bannedUntil, &um.ChatCount, &um.ThreadCount, &lastActive); err != nil {
			return nil, err
		}
		um.Role = Role(role)
		um.IsApproved = approvedAt > 0
		if bannedUntil > 0 {
			t := time.Unix(bannedUntil, 0)
			um.BannedUntil = &t
		}
		if lastActive.Valid {
			t := time.Unix(lastActive.Int64, 0)
			um.LastActive = &t
		}
		out = append(out, um)
	}
	return out, rows.Err()
}

// SetUserStatus flips a user's account status. The super-admin uses this to
// disable (status=disabled) or re-enable (status=active) an account
// platform-wide; auth.Loader logs out any non-active user on their next
// request. Returns ErrNotFound when no such user.
func (r *Repo) SetUserStatus(ctx context.Context, userID string, status UserStatus) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), time.Now().Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- account erasure (GDPR self-serve delete) ---

// deletedSentinelHash is parked in password_hash for an erased account. It is
// not a valid bcrypt hash, so CheckPassword always returns false — password
// login is dead for the tombstone (status=disabled also signs them out).
const deletedSentinelHash = "deleted-account-no-login"

// CommunityRef is a minimal (id, slug, name) tuple for user-facing messages
// (e.g. "you still own these communities").
type CommunityRef struct {
	ID   string
	Slug string
	Name string
}

// SoleOwnerBlockers returns the communities the user is the ONLY owner of that
// still have other members. Account erasure is refused while any exist: the
// user must hand off ownership (or delete the community) first, so a live
// community full of other members is never silently destroyed.
func (r *Repo) SoleOwnerBlockers(ctx context.Context, userID string) ([]CommunityRef, error) {
	return r.ownedCommunities(ctx, userID, `
		  AND (SELECT COUNT(*) FROM memberships m WHERE m.community_id = c.id) > 1`)
}

// SoloOwnedCommunityIDs returns the communities the user solely owns AND is the
// only member of. These hold no other members' data, so erasure deletes them
// outright (via provision.Service.Delete) rather than blocking.
func (r *Repo) SoloOwnedCommunityIDs(ctx context.Context, userID string) ([]string, error) {
	refs, err := r.ownedCommunities(ctx, userID, `
		  AND (SELECT COUNT(*) FROM memberships m WHERE m.community_id = c.id) = 1`)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(refs))
	for i, ref := range refs {
		ids[i] = ref.ID
	}
	return ids, nil
}

// ownedCommunities lists communities the user is the SOLE owner of, narrowed by
// an extra member-count predicate. The shared core keeps the two callers above
// from drifting apart.
func (r *Repo) ownedCommunities(ctx context.Context, userID, memberPredicate string) ([]CommunityRef, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name
		FROM communities c
		JOIN memberships me ON me.community_id = c.id AND me.user_id = ? AND me.role = ?
		WHERE (SELECT COUNT(*) FROM memberships o WHERE o.community_id = c.id AND o.role = ?) = 1`+
		memberPredicate+`
		ORDER BY c.name`, userID, string(RoleOwner), string(RoleOwner))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityRef
	for rows.Next() {
		var ref CommunityRef
		if err := rows.Scan(&ref.ID, &ref.Slug, &ref.Name); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// EraseUser hard-deletes the user's content, memberships, identity links and
// personal rows across every community, then anonymises the users row to a
// tombstone (email scrubbed to an unusable address, password sentinel, status
// disabled) — all in one transaction.
//
// The users row is KEPT, not deleted: shared community artifacts (projects,
// issues, lobbies, mailbox entries, time budgets) FK-reference users(id) with
// RESTRICT, so they survive authored by "deleted user" rather than blocking the
// erase or cascading away other members' work. The content deletes fire the RAG
// embed_outbox AFTER DELETE triggers, so the vector index drops the user's
// chunks on the next worker tick. Owned uploads (blobs) are handled by the
// caller via uploads.Store.DeleteByOwner — they can't be removed inside this tx
// because blob deletion is a filesystem/S3 side effect.
func (r *Repo) EraseUser(ctx context.Context, userID string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// child→parent / content-first order so no FK (787) trips. PM threads are
	// 2-party with ON DELETE CASCADE, so deleting the user's threads also clears
	// their messages + read markers on both sides.
	deletes := []string{
		// public content — also enqueues RAG deletes via AFTER DELETE triggers.
		// chat_messages.author_id is ON DELETE SET NULL, so it MUST be deleted
		// explicitly (a user-row delete would only null it).
		`DELETE FROM chat_messages WHERE author_id = ?`,
		`DELETE FROM posts WHERE author_id = ?`,
		`DELETE FROM threads WHERE author_id = ?`,
		// presence across every community
		`DELETE FROM memberships WHERE user_id = ?`,
		// auth + federated identity (kills magic-link tokens and OAuth links)
		`DELETE FROM verification_tokens WHERE user_id = ?`,
		`DELETE FROM user_identities WHERE user_id = ?`,
		`DELETE FROM signup_tokens WHERE user_id = ?`,
		// personal, single-column
		`DELETE FROM bookmarks WHERE user_id = ?`,
		`DELETE FROM todos WHERE user_id = ?`,
		`DELETE FROM timer_sessions WHERE user_id = ?`,
		`DELETE FROM chat_reads WHERE user_id = ?`,
		`DELETE FROM push_subscriptions WHERE user_id = ?`,
		`DELETE FROM push_pending WHERE user_id = ?`,
	}
	for _, q := range deletes {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return fmt.Errorf("erase user (%s): %w", q, err)
		}
	}
	// two-column relations
	pairs := []string{
		`DELETE FROM user_blocks WHERE blocker_id = ? OR blocked_id = ?`,
		`DELETE FROM user_reports WHERE reporter_id = ? OR reported_user_id = ?`,
		`DELETE FROM private_threads WHERE initiator_user_id = ? OR recipient_user_id = ?`,
	}
	for _, q := range pairs {
		if _, err := tx.ExecContext(ctx, q, userID, userID); err != nil {
			return fmt.Errorf("erase user (%s): %w", q, err)
		}
	}
	// anonymise the surviving row — email gone, login impossible, signed out.
	tombstone := "deleted-" + userID + "@deleted.invalid"
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET email = ?, password_hash = ?, status = ?, updated_at = ?
		WHERE id = ?`,
		tombstone, deletedSentinelHash, string(StatusDisabled), time.Now().Unix(), userID); err != nil {
		return fmt.Errorf("anonymise user: %w", err)
	}
	return tx.Commit()
}

// --- user blocks (per-viewer chat mute) ---

// BlockUser records that blockerID no longer wants to see blockedID's
// chat in this community. Idempotent (INSERT OR IGNORE).
func (r *Repo) BlockUser(ctx context.Context, blockerID, blockedID, communityID string) error {
	if blockerID == "" || blockedID == "" || blockerID == blockedID {
		return nil
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO user_blocks (blocker_id, blocked_id, community_id, created_at)
		VALUES (?, ?, ?, ?)`,
		blockerID, blockedID, communityID, time.Now().Unix())
	return err
}

// UnblockUser removes a block row. No-op when none exists.
func (r *Repo) UnblockUser(ctx context.Context, blockerID, blockedID, communityID string) error {
	_, err := r.DB.ExecContext(ctx, `
		DELETE FROM user_blocks
		WHERE blocker_id = ? AND blocked_id = ? AND community_id = ?`,
		blockerID, blockedID, communityID)
	return err
}

// ListBlocked returns the user_ids blockerID has blocked in the
// community. Empty blockerID returns nil without hitting the DB.
func (r *Repo) ListBlocked(ctx context.Context, blockerID, communityID string) ([]string, error) {
	if blockerID == "" {
		return nil, nil
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT blocked_id FROM user_blocks
		WHERE blocker_id = ? AND community_id = ?`, blockerID, communityID)
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

// --- user reports (moderation queue) ---

// UserReport is one row of the moderation queue, decorated with the
// reporter's and reported user's community display names for the admin UI.
type UserReport struct {
	ID             string
	ReporterID     string
	ReporterName   string
	ReportedUserID string
	ReportedName   string
	Reason         string
	ContextRef     string
	Status         string
	CreatedAt      time.Time
}

// CreateUserReport files a report. Caller supplies the id (uuid).
func (r *Repo) CreateUserReport(ctx context.Context, id, reporterID, reportedUserID, communityID, reason, contextRef string) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO user_reports (id, reporter_id, reported_user_id, community_id, reason, context_ref, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?)`,
		id, reporterID, reportedUserID, communityID, reason, contextRef, time.Now().Unix())
	return err
}

// ListOpenReports returns open reports for the community, newest first,
// with display names resolved from memberships in the same community.
func (r *Repo) ListOpenReports(ctx context.Context, communityID string) ([]UserReport, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT ur.id, ur.reporter_id, COALESCE(rm.display_name, ''),
		       ur.reported_user_id, COALESCE(tm.display_name, ''),
		       ur.reason, ur.context_ref, ur.status, ur.created_at
		FROM user_reports ur
		LEFT JOIN memberships rm ON rm.user_id = ur.reporter_id      AND rm.community_id = ur.community_id
		LEFT JOIN memberships tm ON tm.user_id = ur.reported_user_id AND tm.community_id = ur.community_id
		WHERE ur.community_id = ? AND ur.status = 'open'
		ORDER BY ur.created_at DESC`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserReport
	for rows.Next() {
		var ur UserReport
		var created int64
		if err := rows.Scan(&ur.ID, &ur.ReporterID, &ur.ReporterName,
			&ur.ReportedUserID, &ur.ReportedName, &ur.Reason, &ur.ContextRef, &ur.Status, &created); err != nil {
			return nil, err
		}
		ur.CreatedAt = time.Unix(created, 0)
		out = append(out, ur)
	}
	return out, rows.Err()
}

// ResolveUserReport marks a report resolved.
func (r *Repo) ResolveUserReport(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE user_reports SET status = 'resolved' WHERE id = ?`, id)
	return err
}

// CountAdmins returns the number of privileged (admin OR owner) memberships in
// the community. Used as a last-privileged-member-standing guard before
// remove/demote so an op can't accidentally lock everyone out. Owners count
// because a SaaS community is typically owned by one owner with no separate
// admin (see migration 00055, which promotes the first admin to owner).
func (r *Repo) CountAdmins(ctx context.Context, communityID string) (int, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships
		WHERE community_id = ? AND role IN (?, ?)`,
		communityID, string(RoleAdmin), string(RoleOwner)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountOwnedByUser returns how many communities the user owns (role=owner).
// It is the SaaS self-serve quota gate: a user with zero owned communities may
// create one instantly; beyond that they must request super-admin approval.
func (r *Repo) CountOwnedByUser(ctx context.Context, userID string) (int, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships
		WHERE user_id = ? AND role = ?`,
		userID, string(RoleOwner)).Scan(&n); err != nil {
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
