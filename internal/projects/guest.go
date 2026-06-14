package projects

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var ErrInviteInactive = errors.New("projects: invite is no longer valid")

const (
	sessKeyProjectGuestID        = "project_guest_id"
	sessKeyProjectGuestName      = "project_guest_name"
	sessKeyProjectGuestProjectID = "project_guest_project_id"
)

// ParseGuestTTL converts the form select value (`1h`/`24h`/`7d`/`0`)
// into a duration. `0` means no-expire (returns 0, caller treats as
// nil expiry).
func ParseGuestTTL(s string) (time.Duration, error) {
	switch s {
	case "1h":
		return time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "0", "":
		return 0, nil
	}
	return 0, fmt.Errorf("projects: bad TTL %q", s)
}

// MintGuestInvite revokes any active invite for the project, then
// inserts a fresh one with the requested TTL. ttl=0 means no expiry.
func (s *Service) MintGuestInvite(ctx context.Context, projectID, createdBy string, ttl time.Duration, callerUserID string, callerIsAdmin bool) (GuestInvite, error) {
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return GuestInvite{}, fmt.Errorf("project lookup: %w", err)
	}
	if !(callerIsAdmin || p.CreatorUserID == callerUserID) {
		return GuestInvite{}, ErrForbidden
	}
	now := time.Now().UTC()
	if prev, err := s.Repo.ActiveGuestInviteForProject(ctx, projectID); err == nil {
		_ = s.Repo.RevokeGuestInvite(ctx, prev.Token, now)
	}
	token, err := randToken(32)
	if err != nil {
		return GuestInvite{}, err
	}
	g := GuestInvite{
		Token:     token,
		ProjectID: projectID,
		CreatedBy: createdBy,
		CreatedAt: now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		g.ExpiresAt = &exp
	}
	if err := s.Repo.CreateGuestInvite(ctx, g); err != nil {
		return GuestInvite{}, err
	}
	return g, nil
}

// RevokeGuestInvite cancels the active token for a project. Admin or
// creator only.
func (s *Service) RevokeActiveGuestInvite(ctx context.Context, projectID, callerUserID string, callerIsAdmin bool) error {
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project lookup: %w", err)
	}
	if !(callerIsAdmin || p.CreatorUserID == callerUserID) {
		return ErrForbidden
	}
	inv, err := s.Repo.ActiveGuestInviteForProject(ctx, projectID)
	if err != nil {
		return nil // no active invite — nothing to do
	}
	return s.Repo.RevokeGuestInvite(ctx, inv.Token, time.Now().UTC())
}

// RedeemGuestInvite validates a token and returns the project + a
// fresh guest identity. Caller persists the identity in the session.
func (s *Service) RedeemGuestInvite(ctx context.Context, token, displayName string) (Project, Identity, error) {
	inv, err := s.Repo.GuestInviteByToken(ctx, token)
	if err != nil {
		return Project{}, Identity{}, ErrInviteInactive
	}
	if !inv.Active(time.Now().UTC()) {
		return Project{}, Identity{}, ErrInviteInactive
	}
	p, err := s.Repo.ByID(ctx, inv.ProjectID)
	if err != nil {
		return Project{}, Identity{}, fmt.Errorf("project lookup: %w", err)
	}
	gid := newGuestID()
	id := Identity{GuestID: gid, Name: displayName}
	return p, id, nil
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newGuestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
