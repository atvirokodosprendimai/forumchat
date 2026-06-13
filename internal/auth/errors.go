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
	ErrTokenInvalid       = errors.New("verification token invalid or expired")
	ErrNotFound           = errors.New("not found")
)
