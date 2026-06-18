package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	Repo      *Repo
	Mailer    Mailer
	BaseURL   string
	VerifyTTL time.Duration
	InviteTTL time.Duration

	// OpenRegistration allows Register to proceed without an invite code.
	OpenRegistration bool
	// OpenRegistrationAutoApprove stamps approved_at at verify time so open
	// registrants skip the pending queue. Only honoured when OpenRegistration.
	OpenRegistrationAutoApprove bool
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

type RegisterAsAdminInput struct {
	Email       string
	Password    string
	DisplayName string
}

type RegisterAsAdminResult struct {
	UserID      string
	CommunityID string
}

// RegisterAsAdmin is the no-invite bootstrap flow used only when the
// database has zero users. It creates an active (already-verified)
// admin membership in the supplied community so the operator can sign
// in immediately and start issuing invites.
//
// The caller MUST guard this with a `users == 0` check first; this
// function re-checks inside the same transaction to close the race
// window but only by best effort — sqlite locking is enough for a
// single-process deployment.
func (s *Service) RegisterAsAdmin(ctx context.Context, in RegisterAsAdminInput, communityID string) (RegisterAsAdminResult, error) {
	hash, err := HashPassword(in.Password)
	if err != nil {
		return RegisterAsAdminResult{}, err
	}
	tx, err := s.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return RegisterAsAdminResult{}, err
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
		return RegisterAsAdminResult{}, err
	}
	if existing > 0 {
		return RegisterAsAdminResult{}, ErrInviteInvalid // re-use a generic refusal; caller surfaces it
	}

	userID := uuid.NewString()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, normEmail(in.Email), hash, string(StatusActive), now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return RegisterAsAdminResult{}, ErrEmailTaken
		}
		return RegisterAsAdminResult{}, fmt.Errorf("insert user: %w", err)
	}

	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = localPart(in.Email)
	}
	approved := time.Now()
	m := Membership{
		ID:          uuid.NewString(),
		UserID:      userID,
		CommunityID: communityID,
		DisplayName: displayName,
		Role:        RoleAdmin,
		TrustLevel:  0,
		ApprovedAt:  &approved,
	}
	if err := s.Repo.CreateMembership(ctx, tx, m); err != nil {
		return RegisterAsAdminResult{}, fmt.Errorf("create membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RegisterAsAdminResult{}, err
	}
	return RegisterAsAdminResult{UserID: userID, CommunityID: communityID}, nil
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

// MagicLoginTTL is the default lifetime of a magic-login email link. Kept
// short — the link grants a session without a password.
const MagicLoginTTL = 30 * time.Minute

// IssueMagicLink emails a one-shot login URL to the address. Reuses the
// verification_tokens table with purpose='magic_login'. Returns nil even
// when the email maps to no account — callers must not branch on the
// result to avoid revealing membership (account-enumeration defence).
//
// Activated and pending accounts both receive a link; the consume step
// activates pending users since the magic link is itself proof of
// email ownership (same trust level as the registration verify mail).
// Disabled / banned accounts get no link.
func (s *Service) IssueMagicLink(ctx context.Context, email string) error {
	u, err := s.Repo.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // silent no-op
		}
		return err
	}
	if u.Status == StatusDisabled {
		return nil
	}
	token, err := RandomToken(24)
	if err != nil {
		return err
	}
	exp := time.Now().Add(MagicLoginTTL)
	if err := s.Repo.CreateVerificationToken(ctx, token, u.ID, "magic_login", exp); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/login/magic?token=%s", s.BaseURL, token)
	body := fmt.Sprintf("Sign in to forumchat by clicking the link below.\n\n%s\n\nLink expires %s.\nIgnore this email if you didn't request a login.\n",
		url, exp.Format(time.RFC1123))
	if err := s.Mailer.Send(ctx, email, "Sign in to forumchat", body); err != nil {
		// Don't propagate mail failures — the token still exists; UX shows
		// "check your email" either way and ops fixes the mailer.
		return nil
	}
	return nil
}

// ConsumeMagicLink swaps a magic-login token for a LoginResult. Activates
// the user if still pending (the token mail proves email ownership).
// Auto-joins the supplied community on first sign-in, matching the
// register/verify flow.
func (s *Service) ConsumeMagicLink(ctx context.Context, token, communityID string) (LoginResult, error) {
	vt, err := s.Repo.ConsumeVerificationToken(ctx, token, "magic_login")
	if err != nil {
		return LoginResult{}, err
	}
	u, err := s.Repo.UserByID(ctx, vt.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	if u.Status == StatusDisabled {
		return LoginResult{}, ErrBanned
	}
	if u.Status == StatusPending || u.Status == StatusInvited {
		if err := s.Repo.ActivateUser(ctx, u.ID); err != nil {
			return LoginResult{}, err
		}
		u.Status = StatusActive
	}
	m, err := s.Repo.MembershipFor(ctx, u.ID, communityID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return LoginResult{}, err
		}
		m = Membership{
			ID:          uuid.NewString(),
			UserID:      u.ID,
			CommunityID: communityID,
			DisplayName: localPart(u.Email),
			Role:        RoleMember,
		}
		if err := s.Repo.CreateMembership(ctx, nil, m); err != nil {
			if !isUniqueViolation(err) {
				return LoginResult{}, err
			}
		}
	}
	if m.IsBanned(time.Now()) {
		return LoginResult{}, ErrBanned
	}
	return LoginResult{User: u, Membership: m}, nil
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
	if u.Status == StatusPending || u.Status == StatusInvited {
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

// IssueInvite creates a new invite. maxUses=nil → unlimited (Discord-style);
// a value of N caps the invite after that many consumers.
func (s *Service) IssueInvite(ctx context.Context, communityID string, createdBy *string, maxUses *int) (string, error) {
	code, err := InviteCodeText()
	if err != nil {
		return "", err
	}
	if err := s.Repo.CreateInvite(ctx, code, communityID, createdBy, maxUses, time.Now().Add(s.InviteTTL)); err != nil {
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
