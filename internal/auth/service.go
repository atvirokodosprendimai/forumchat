package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
	Log       *slog.Logger

	// CommunityID is the bootstrap community new members join when no invite
	// supplies one (open registration / auto-verify paths).
	CommunityID string
	// OpenRegistration allows Register to proceed without an invite code.
	OpenRegistration bool
	// OpenRegistrationAutoApprove stamps approved_at at verify time so new
	// members skip the pending queue. Applies to open AND invite-based signups.
	OpenRegistrationAutoApprove bool
	// AutoVerifyEmail skips the email round-trip: Register activates the user
	// and creates their membership immediately (handy for short demo windows).
	AutoVerifyEmail bool

	// Communities deletes the solo-owned communities found during account
	// erasure (provision.Service satisfies CommunityDeleter). Declared as an
	// interface so auth doesn't import internal/provision — which imports auth,
	// and would cycle. Nil tolerated (erasure then just drops memberships).
	Communities CommunityDeleter
	// Uploads purges a user's owned blobs during erasure (uploads.Store
	// satisfies UploadPurger). Nil tolerated.
	Uploads UploadPurger
}

// CommunityDeleter purges a whole community — blobs, cascaded rows and vectors.
// provision.Service.Delete satisfies it.
type CommunityDeleter interface {
	Delete(ctx context.Context, communityID string) error
}

// UploadPurger removes every upload a user owns (rows + blobs).
// *uploads.Store.DeleteByOwner satisfies it.
type UploadPurger interface {
	DeleteByOwner(ctx context.Context, ownerID string) (int, error)
}

// SoleOwnerError is returned by DeleteAccount when the user still solely owns
// one or more communities that have OTHER members. Nothing is deleted; the user
// must hand off ownership (or delete those communities) first. Blockers names
// them for the UI.
type SoleOwnerError struct{ Blockers []CommunityRef }

func (e *SoleOwnerError) Error() string {
	return fmt.Sprintf("account is the sole owner of %d community/ies with other members", len(e.Blockers))
}

// DeleteLinkTTL is the lifetime of an account-deletion confirmation link.
const DeleteLinkTTL = 30 * time.Minute

// CheckDeletable returns the communities that currently BLOCK erasure (sole
// owner + other members). Empty slice means the account can be deleted. Used by
// the password step to warn the user before any email is sent.
func (s *Service) CheckDeletable(ctx context.Context, userID string) ([]CommunityRef, error) {
	return s.Repo.SoleOwnerBlockers(ctx, userID)
}

// IssueDeletionLink mints a one-time account_delete token and emails the
// confirmation link. The caller has already proven the password; this is step 2
// of the two-step erase. Token/DB errors are returned (the UI should tell the
// user the mail didn't go out); a pure mailer failure is logged but swallowed
// (the token still exists).
func (s *Service) IssueDeletionLink(ctx context.Context, userID, email string) error {
	token, err := RandomToken(24)
	if err != nil {
		return err
	}
	exp := time.Now().Add(DeleteLinkTTL)
	if err := s.Repo.CreateVerificationToken(ctx, token, userID, "account_delete", exp); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/profile/delete/confirm?token=%s", s.BaseURL, token)
	body := fmt.Sprintf("You asked to permanently delete your account.\n\nConfirm by opening the link below — this is IRREVERSIBLE and erases your data across every community:\n\n%s\n\nThe link expires %s. If you did not request this, ignore this email and change your password.\n",
		url, exp.Format(time.RFC1123))
	if err := s.Mailer.Send(ctx, email, "Confirm account deletion", body); err != nil {
		if s.Log != nil {
			s.Log.Error("send deletion link", "err", err)
		}
	}
	return nil
}

// DeleteAccount runs the irreversible erasure: guard sole-ownership, delete the
// communities the user solely owns and is the only member of, purge their
// uploads, then erase + anonymise the account (Repo.EraseUser). Returns a
// *SoleOwnerError (nothing deleted) when a shared community must be handed off
// first.
func (s *Service) DeleteAccount(ctx context.Context, userID string) error {
	blockers, err := s.Repo.SoleOwnerBlockers(ctx, userID)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return &SoleOwnerError{Blockers: blockers}
	}
	// Delete the user's solo communities BEFORE the membership rows are erased,
	// so the ownership is still visible. Each Delete purges that community's
	// blobs + rows + vectors.
	if s.Communities != nil {
		solo, err := s.Repo.SoloOwnedCommunityIDs(ctx, userID)
		if err != nil {
			return err
		}
		for _, cid := range solo {
			if err := s.Communities.Delete(ctx, cid); err != nil {
				return fmt.Errorf("delete solo community %s: %w", cid, err)
			}
		}
	}
	// Owned upload blobs across the remaining communities. The users row is kept
	// (anonymised), so the uploads.owner_id cascade won't fire — remove them
	// explicitly. Best-effort: a leftover blob is a leak, not a correctness bug.
	if s.Uploads != nil {
		if _, err := s.Uploads.DeleteByOwner(ctx, userID); err != nil && s.Log != nil {
			s.Log.Error("purge user uploads on erase", "user_id", userID, "err", err)
		}
	}
	return s.Repo.EraseUser(ctx, userID)
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
	// AutoVerified is true when AutoVerifyEmail skipped the email step and the
	// user is already active + a member — the handler should log them straight in.
	AutoVerified bool
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

	// Invite is optional only when open registration is on. With a code we
	// always consume it; without one we either join the bootstrap community
	// (open) or refuse (invite-only).
	communityID := s.CommunityID
	if in.InviteCode != "" {
		invite, err := s.Repo.ConsumeInvite(ctx, tx, in.InviteCode, userID)
		if err != nil {
			return RegisterResult{}, err
		}
		communityID = invite.CommunityID
	} else if !s.OpenRegistration {
		return RegisterResult{}, ErrInviteRequired
	}

	// AutoVerifyEmail short-circuits the verification email: commit the user
	// (+ invite consume), then activate and join immediately so the handler can
	// sign the new member straight in.
	if s.AutoVerifyEmail {
		if err := tx.Commit(); err != nil {
			return RegisterResult{}, err
		}
		if err := s.activateAndJoin(ctx, userID, communityID, in.Email); err != nil {
			return RegisterResult{}, err
		}
		return RegisterResult{UserID: userID, CommunityID: communityID, AutoVerified: true}, nil
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
		// Don't fail registration if mail fails (e.g. SMTP unreachable) — the
		// token is valid. Log the verify URL so an operator can recover by
		// visiting it manually instead of silently swallowing the failure.
		if s.Log != nil {
			s.Log.Warn("verify email send failed; visit verify_url to verify manually",
				"to", in.Email, "verify_url", verifyURL, "err", err)
		}
	}

	return RegisterResult{
		UserID:            userID,
		CommunityID:       communityID,
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
	u, err := s.Repo.UserByID(ctx, vt.UserID)
	if err != nil {
		return VerifyResult{}, err
	}
	if err := s.activateAndJoin(ctx, vt.UserID, communityID, u.Email); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{UserID: vt.UserID, CommunityID: communityID}, nil
}

// activateAndJoin marks the user active and creates their membership in the
// community, auto-approved when configured. Shared by Verify (email link) and
// by Register when AutoVerifyEmail skips the email round-trip. Re-running it for
// an existing member is tolerated (unique violation ignored).
func (s *Service) activateAndJoin(ctx context.Context, userID, communityID, email string) error {
	if err := s.Repo.ActivateUser(ctx, userID); err != nil {
		return err
	}
	m := Membership{
		ID:          uuid.NewString(),
		UserID:      userID,
		CommunityID: communityID,
		DisplayName: localPart(email),
		Role:        RoleMember,
		TrustLevel:  0,
	}
	// Auto-approve stamps approved_at now so the member skips the pending
	// queue. Honoured whenever the flag is set — for open OR invite-based
	// signups (an admin who turns this on wants no manual approval step).
	if s.OpenRegistrationAutoApprove {
		t := time.Now()
		m.ApprovedAt = &t
	}
	if err := s.Repo.CreateMembership(ctx, nil, m); err != nil {
		if !isUniqueViolation(err) {
			return err
		}
	}
	return nil
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

// OAuthInput is the slice of a provider profile UpsertOAuthUser needs.
type OAuthInput struct {
	Provider       string
	ProviderUserID string
	Email          string
	Name           string
	AvatarURL      string
	// EmailVerified is whether the provider proved the user owns Email. Only a
	// verified email may auto-link to an existing local (password) account —
	// otherwise an attacker could register a provider account with a victim's
	// unverified email and take over the victim's account. Set per provider in
	// the handler.
	EmailVerified bool
}

// UpsertOAuthUser resolves a provider sign-in to a LoginResult. Resolution
// order:
//  1. a known (provider, provider_user_id) → that user (the fast path for
//     returning users).
//  2. an existing local user with the same (provider-verified) email → link
//     the identity and sign in.
//  3. a brand-new email → create the account, but only when OpenRegistration
//     is on; otherwise refuse with ErrOAuthNoAccount. This mirrors the invite
//     gate on the email/password path — OAuth is a login method, not a way to
//     bypass closed registration.
//
// New accounts are created active (the provider verified the email) and join
// the bootstrap community via finishOAuth, landing in the approval queue unless
// OpenRegistrationAutoApprove is set.
func (s *Service) UpsertOAuthUser(ctx context.Context, in OAuthInput) (LoginResult, error) {
	in.Email = normEmail(in.Email)
	if in.Email == "" {
		return LoginResult{}, ErrOAuthNoEmail
	}

	// 1. Returning user — identity already linked.
	if uid, err := s.Repo.UserIDByIdentity(ctx, in.Provider, in.ProviderUserID); err == nil {
		u, err := s.Repo.UserByID(ctx, uid)
		if err != nil {
			return LoginResult{}, err
		}
		return s.finishOAuth(ctx, u, in)
	} else if !errors.Is(err, ErrNotFound) {
		return LoginResult{}, err
	}

	// 2. Existing local account with the same email — link ONLY when the
	//    provider proved the email. An unverified match is an account-takeover
	//    vector (register a provider account with the victim's email), so refuse
	//    it and let them sign in with their password instead.
	if u, err := s.Repo.UserByEmail(ctx, in.Email); err == nil {
		if u.Status == StatusDisabled {
			return LoginResult{}, ErrBanned
		}
		if !in.EmailVerified {
			return LoginResult{}, ErrOAuthEmailUnverified
		}
		if err := s.Repo.LinkIdentity(ctx, nil, identityFrom(in, u.ID)); err != nil {
			return LoginResult{}, err
		}
		return s.finishOAuth(ctx, u, in)
	} else if !errors.Is(err, ErrNotFound) {
		return LoginResult{}, err
	}

	// 3. Brand-new email — only self-onboard when open registration is on.
	if !s.OpenRegistration {
		return LoginResult{}, ErrOAuthNoAccount
	}
	userID := uuid.NewString()
	tx, err := s.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return LoginResult{}, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, in.Email, oauthSentinelHash, string(StatusActive), now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return LoginResult{}, ErrEmailTaken
		}
		return LoginResult{}, fmt.Errorf("insert oauth user: %w", err)
	}
	if err := s.Repo.LinkIdentity(ctx, tx, identityFrom(in, userID)); err != nil {
		return LoginResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return LoginResult{}, err
	}
	u, err := s.Repo.UserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}
	return s.finishOAuth(ctx, u, in)
}

func identityFrom(in OAuthInput, userID string) OAuthIdentity {
	return OAuthIdentity{
		Provider:       in.Provider,
		ProviderUserID: in.ProviderUserID,
		UserID:         userID,
		Email:          in.Email,
		Name:           in.Name,
		AvatarURL:      in.AvatarURL,
	}
}

// finishOAuth activates a pending user, ensures a membership in the bootstrap
// community (creating one — seeded with the provider's display name + avatar —
// when absent), and returns the LoginResult. Disabled / banned accounts are
// refused with ErrBanned.
func (s *Service) finishOAuth(ctx context.Context, u User, in OAuthInput) (LoginResult, error) {
	if u.Status == StatusDisabled {
		return LoginResult{}, ErrBanned
	}
	if u.Status == StatusPending || u.Status == StatusInvited {
		if err := s.Repo.ActivateUser(ctx, u.ID); err != nil {
			return LoginResult{}, err
		}
		u.Status = StatusActive
	}
	m, err := s.Repo.MembershipFor(ctx, u.ID, s.CommunityID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return LoginResult{}, err
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = localPart(u.Email)
		}
		m = Membership{
			ID:          uuid.NewString(),
			UserID:      u.ID,
			CommunityID: s.CommunityID,
			DisplayName: name,
			AvatarURL:   in.AvatarURL,
			Role:        RoleMember,
		}
		if s.OpenRegistrationAutoApprove {
			t := time.Now()
			m.ApprovedAt = &t
		}
		if err := s.Repo.CreateMembership(ctx, nil, m); err != nil {
			if !isUniqueViolation(err) {
				return LoginResult{}, err
			}
			// Raced with a concurrent sign-in that created the row — re-read it.
			if m, err = s.Repo.MembershipFor(ctx, u.ID, s.CommunityID); err != nil {
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
			CheckPasswordDummy(password) // equalize timing vs the bcrypt path below
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
