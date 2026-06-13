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
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

func (r *Repo) CreateInvite(ctx context.Context, code, communityID string, createdBy *string, expiresAt time.Time) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO invite_codes (code, community_id, created_by, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		code, communityID, createdBy, expiresAt.Unix(), time.Now().Unix())
	return err
}

func (r *Repo) ConsumeInvite(ctx context.Context, tx *sql.Tx, code, userID string) (InviteCode, error) {
	var ic InviteCode
	var createdBy, usedBy sql.NullString
	var usedAt sql.NullInt64
	var exp, created int64
	err := tx.QueryRowContext(ctx, `
		SELECT code, community_id, created_by, used_by, used_at, expires_at, created_at
		FROM invite_codes WHERE code = ?`, code).
		Scan(&ic.Code, &ic.CommunityID, &createdBy, &usedBy, &usedAt, &exp, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteCode{}, ErrInviteInvalid
	}
	if err != nil {
		return InviteCode{}, err
	}
	if createdBy.Valid {
		ic.CreatedBy = &createdBy.String
	}
	ic.ExpiresAt = time.Unix(exp, 0)
	ic.CreatedAt = time.Unix(created, 0)
	if usedBy.Valid {
		return InviteCode{}, ErrInviteUsed
	}
	if time.Now().After(ic.ExpiresAt) {
		return InviteCode{}, ErrInviteInvalid
	}
	_, err = tx.ExecContext(ctx, `UPDATE invite_codes SET used_by = ?, used_at = ? WHERE code = ?`,
		userID, time.Now().Unix(), code)
	if err != nil {
		return InviteCode{}, err
	}
	return ic, nil
}

// --- memberships ---

func (r *Repo) CreateMembership(ctx context.Context, tx *sql.Tx, m Membership) error {
	exec := r.DB.ExecContext
	if tx != nil {
		exec = tx.ExecContext
	}
	var banned sql.NullInt64
	if m.BannedUntil != nil {
		banned = sql.NullInt64{Int64: m.BannedUntil.Unix(), Valid: true}
	}
	_, err := exec(ctx, `
		INSERT INTO memberships (id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.UserID, m.CommunityID, m.DisplayName, m.AvatarURL, string(m.Role), m.TrustLevel, banned, time.Now().Unix())
	return err
}

func (r *Repo) MembershipFor(ctx context.Context, userID, communityID string) (Membership, error) {
	var m Membership
	var role string
	var banned sql.NullInt64
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, user_id, community_id, display_name, avatar_url, role, trust_level, banned_until, created_at
		FROM memberships WHERE user_id = ? AND community_id = ?`, userID, communityID).
		Scan(&m.ID, &m.UserID, &m.CommunityID, &m.DisplayName, &m.AvatarURL, &role, &m.TrustLevel, &banned, &created)
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
	m.CreatedAt = time.Unix(created, 0)
	return m, nil
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
