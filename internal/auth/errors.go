package auth

import "errors"

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrWeakPassword       = errors.New("password must be at least 8 characters")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrNotVerified        = errors.New("account not verified")
	ErrBanned             = errors.New("account banned")
	ErrInviteInvalid      = errors.New("invite code invalid or expired")
	ErrInviteUsed         = errors.New("invite code already used")
	ErrInviteExhausted    = errors.New("invite code has no remaining uses")
	ErrInviteRequired     = errors.New("invite code required")
	ErrTokenInvalid       = errors.New("verification token invalid or expired")
	ErrPendingApproval    = errors.New("membership awaiting admin approval")
	ErrNotFound           = errors.New("not found")
	ErrOAuthNoEmail       = errors.New("oauth account shared no email")
	ErrOAuthNoAccount     = errors.New("no account registered for this email")
)
