package auth

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

// SignupToken is the row backing a per-community add-by-email invitation. It
// is consumed by the join page (Handler in internal/invites) and is single-use.
type SignupToken struct {
	Token       string
	UserID      string
	CommunityID string
	ExpiresAt   time.Time
	UsedAt      *time.Time
	CreatedAt   time.Time
}

func (t SignupToken) IsUsed() bool    { return t.UsedAt != nil }
func (t SignupToken) IsExpired() bool { return time.Now().After(t.ExpiresAt) }
func (t SignupToken) IsValid() bool   { return !t.IsUsed() && !t.IsExpired() }

// CreateInvitedUser inserts a placeholder users row for an admin-initiated
// invite. password_hash is empty (login is gated on status='active'), status
// is 'invited'. Caller follows with CreateMembership + MintSignupToken.
func (r *Repo) CreateInvitedUser(ctx context.Context, email string) (User, error) {
	u := User{
		ID:           uuid.NewString(),
		Email:        normEmail(email),
		PasswordHash: "",
		Status:       StatusInvited,
	}
	if err := r.CreateUser(ctx, u); err != nil {
		return User{}, err
	}
	now := time.Now()
	u.CreatedAt = now
	u.UpdatedAt = now
	return u, nil
}

// SetPasswordAndActivate stamps a new password hash, flips status to active,
// and bumps updated_at. Used by the join-set-password flow on placeholder
// users.
func (r *Repo) SetPasswordAndActivate(ctx context.Context, userID, passwordHash string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE users SET password_hash = ?, status = ?, updated_at = ? WHERE id = ?`,
		passwordHash, string(StatusActive), time.Now().Unix(), userID)
	if err != nil {
		return fmt.Errorf("set password and activate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePassword sets a new password hash on an existing user and bumps
// updated_at. Unlike SetPasswordAndActivate it does NOT touch status — the user
// is already active. Used by the signed-in change/set-password flow.
func (r *Repo) UpdatePassword(ctx context.Context, userID, passwordHash string) error {
	res, err := r.DB.ExecContext(ctx, `
		UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, time.Now().Unix(), userID)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MintSignupToken creates a single-use token bound to userID + communityID,
// valid for ttl. Returns the raw token string.
func (r *Repo) MintSignupToken(ctx context.Context, userID, communityID string, ttl time.Duration) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO signup_tokens (token, user_id, community_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		token, userID, communityID, now.Add(ttl).Unix(), now.Unix())
	if err != nil {
		return "", fmt.Errorf("mint token insert: %w", err)
	}
	return token, nil
}

// SignupTokenByValue returns the row for the given token regardless of used/
// expired state — callers decide how to present each state. ErrNotFound when
// no row matches.
func (r *Repo) SignupTokenByValue(ctx context.Context, token string) (SignupToken, error) {
	row := r.DB.QueryRowContext(ctx, `
		SELECT token, user_id, community_id, expires_at, used_at, created_at
		FROM signup_tokens WHERE token = ?`, token)
	var st SignupToken
	var expires, created int64
	var used sql.NullInt64
	if err := row.Scan(&st.Token, &st.UserID, &st.CommunityID, &expires, &used, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SignupToken{}, ErrNotFound
		}
		return SignupToken{}, err
	}
	st.ExpiresAt = time.Unix(expires, 0)
	st.CreatedAt = time.Unix(created, 0)
	if used.Valid {
		u := time.Unix(used.Int64, 0)
		st.UsedAt = &u
	}
	return st, nil
}

// ConsumeSignupToken marks the token used. Idempotent: a second call on the
// same token is a no-op (returns nil, n=0).
func (r *Repo) ConsumeSignupToken(ctx context.Context, token string) error {
	_, err := r.DB.ExecContext(ctx, `
		UPDATE signup_tokens SET used_at = ? WHERE token = ? AND used_at IS NULL`,
		time.Now().Unix(), token)
	return err
}
