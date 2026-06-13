package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	Repo        *Repo
	Mailer      Mailer
	BaseURL     string
	VerifyTTL   time.Duration
	InviteTTL   time.Duration
}

type RegisterInput struct {
	Email      string
	Password   string
	InviteCode string
}

type RegisterResult struct {
	UserID            string
	CommunityID       string
	VerificationToken string
	VerifyURL         string
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	hash, err := HashPassword(in.Password)
	if err != nil {
		return RegisterResult{}, err
	}
	tx, err := s.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return RegisterResult{}, err
	}
	defer tx.Rollback()

	userID := uuid.NewString()

	// Insert user first so the invite consume's used_by FK is satisfied.
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, normEmail(in.Email), hash, string(StatusPending), now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return RegisterResult{}, ErrEmailTaken
		}
		return RegisterResult{}, fmt.Errorf("insert user: %w", err)
	}

	invite, err := s.Repo.ConsumeInvite(ctx, tx, in.InviteCode, userID)
	if err != nil {
		return RegisterResult{}, err
	}

	token, err := RandomToken(24)
	if err != nil {
		return RegisterResult{}, err
	}
	exp := time.Now().Add(s.VerifyTTL)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO verification_tokens (token, user_id, purpose, expires_at)
		VALUES (?, ?, ?, ?)`, token, userID, "email_verify", exp.Unix(),
	); err != nil {
		return RegisterResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisterResult{}, err
	}

	verifyURL := fmt.Sprintf("%s/verify?token=%s", s.BaseURL, token)
	body := fmt.Sprintf("Welcome to forumchat.\n\nClick to verify your account:\n%s\n\nLink expires %s.\n",
		verifyURL, exp.Format(time.RFC1123))
	if err := s.Mailer.Send(ctx, in.Email, "Verify your forumchat account", body); err != nil {
		// Don't fail registration if mail fails — the token still exists; log only.
	}

	return RegisterResult{
		UserID:            userID,
		CommunityID:       invite.CommunityID,
		VerificationToken: token,
		VerifyURL:         verifyURL,
	}, nil
}

type VerifyResult struct {
	UserID      string
	CommunityID string
}

func (s *Service) Verify(ctx context.Context, token, communityID string) (VerifyResult, error) {
	vt, err := s.Repo.ConsumeVerificationToken(ctx, token, "email_verify")
	if err != nil {
		return VerifyResult{}, err
	}
	if err := s.Repo.ActivateUser(ctx, vt.UserID); err != nil {
		return VerifyResult{}, err
	}
	u, err := s.Repo.UserByID(ctx, vt.UserID)
	if err != nil {
		return VerifyResult{}, err
	}
	displayName := localPart(u.Email)
	// Auto-join provided community (bootstrap community).
	m := Membership{
		ID:          uuid.NewString(),
		UserID:      vt.UserID,
		CommunityID: communityID,
		DisplayName: displayName,
		Role:        RoleMember,
		TrustLevel:  0,
	}
	if err := s.Repo.CreateMembership(ctx, nil, m); err != nil {
		// If already member (re-verify edge case), tolerate.
		if !isUniqueViolation(err) {
			return VerifyResult{}, err
		}
	}
	return VerifyResult{UserID: vt.UserID, CommunityID: communityID}, nil
}

type LoginResult struct {
	User       User
	Membership Membership
}

func (s *Service) Login(ctx context.Context, email, password, communityID string) (LoginResult, error) {
	u, err := s.Repo.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if u.Status == StatusPending {
		return LoginResult{}, ErrNotVerified
	}
	if u.Status == StatusDisabled {
		return LoginResult{}, ErrBanned
	}
	m, err := s.Repo.MembershipFor(ctx, u.ID, communityID)
	if err != nil {
		return LoginResult{}, err
	}
	if m.IsBanned(time.Now()) {
		return LoginResult{}, ErrBanned
	}
	return LoginResult{User: u, Membership: m}, nil
}

func (s *Service) IssueInvite(ctx context.Context, communityID string, createdBy *string) (string, error) {
	code, err := InviteCodeText()
	if err != nil {
		return "", err
	}
	if err := s.Repo.CreateInvite(ctx, code, communityID, createdBy, time.Now().Add(s.InviteTTL)); err != nil {
		return "", err
	}
	return code, nil
}

func localPart(email string) string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			if i == 0 {
				return "user"
			}
			return email[:i]
		}
	}
	return "user"
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite returns errors with "UNIQUE constraint failed" in message.
	return contains(err.Error(), "UNIQUE")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// silence import
var _ = sql.ErrNoRows
